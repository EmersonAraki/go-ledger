package reconcile

import (
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ErrMissingHeader means the CSV had no usable header row.
var ErrMissingHeader = errors.New("statement is empty or has no header row")

// Statement is the result of parsing an uploaded file.
type Statement struct {
	// Rows are the rows that parsed.
	Rows []StatementRow
	// Findings are per-row parse failures and any truncation notice.
	Findings []Discrepancy
	// RowsRead is how many data rows were actually read, which is the honest
	// denominator for a run summary -- len(Rows)+len(Findings) is not, because
	// findings are capped and include synthetic entries.
	RowsRead int
}

// StatementRow is one line of an external statement.
type StatementRow struct {
	// Line is the 1-based line number in the source file, so an operator can
	// find the row a discrepancy refers to.
	Line            int
	ExternalRef     string
	PostedAt        time.Time
	DebitAccountID  uuid.UUID
	CreditAccountID uuid.UUID
	Amount          int64
	Currency        string
}

// requiredColumns is the header the parser expects. Columns may appear in any
// order -- the header is read by name -- but all of these must be present.
var requiredColumns = []string{
	"external_ref", "posted_at", "debit_account_id", "credit_account_id", "amount", "currency",
}

// ParseStatement reads a CSV statement.
//
// Rows are read one at a time rather than with ReadAll, so the raw file and a
// full [][]string never exist at once. A malformed row becomes an
// unparseable_row discrepancy instead of aborting the run: one bad line must not
// hide every other finding in the file.
func ParseStatement(r io.Reader) (Statement, error) {
	reader := csv.NewReader(r)
	reader.FieldsPerRecord = -1 // validated per row, with a useful message
	reader.TrimLeadingSpace = true
	reader.ReuseRecord = true // safe: values are copied out before the next Read

	header, err := reader.Read()
	if errors.Is(err, io.EOF) {
		return Statement{}, ErrMissingHeader
	}
	if err != nil {
		return Statement{}, fmt.Errorf("read header: %w", err)
	}

	index, err := indexHeader(header)
	if err != nil {
		return Statement{}, err
	}

	var (
		rows          []StatementRow
		bad           []Discrepancy
		lineNum       = 1 // the header
		dataRows      int
		suppressedBad int
		truncated     bool
	)
	for {
		record, err := reader.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		lineNum++
		dataRows++

		// Check AFTER reading, not before. Checking first means a file of
		// exactly MaxStatementRows rows is stamped truncated without ever
		// discovering EOF -- a complete comparison reported as incomplete, which
		// in an audit artifact is the wrong direction to be wrong in.
		if dataRows > MaxStatementRows {
			truncated = true
			dataRows--
			break
		}

		if err != nil {
			// A parse error here is structural (bad quoting, stray delimiter).
			// Record it and keep going.
			if len(bad) < MaxReportedParseErrors {
				bad = append(bad, unparseable(lineNum, err.Error()))
			} else {
				suppressedBad++
			}
			var parseErr *csv.ParseError
			if errors.As(err, &parseErr) {
				continue
			}
			// Anything else is an I/O failure; the file cannot be trusted.
			return Statement{}, fmt.Errorf("read statement: %w", err)
		}

		row, err := parseRow(lineNum, record, index)
		if err != nil {
			if len(bad) < MaxReportedParseErrors {
				bad = append(bad, unparseable(lineNum, err.Error()))
			} else {
				suppressedBad++
			}
			continue
		}
		rows = append(rows, row)
	}

	if truncated || suppressedBad > 0 {
		bad = append(bad, truncationFinding(dataRows, suppressedBad, truncated))
	}
	return Statement{Rows: rows, Findings: bad, RowsRead: dataRows}, nil
}

// truncationFinding records that limits were hit, so a partial comparison can
// never be mistaken for a complete one.
func truncationFinding(dataRows, suppressed int, rowLimitHit bool) Discrepancy {
	details := map[string]any{
		"rows_read": dataRows,
	}
	if rowLimitHit {
		details["reason"] = "statement exceeded the row limit and was read only in part"
		details["max_rows"] = MaxStatementRows
	} else {
		details["reason"] = "too many unparseable rows to list individually"
	}
	if suppressed > 0 {
		details["unreported_parse_errors"] = suppressed
		details["max_reported_parse_errors"] = MaxReportedParseErrors
	}
	return Discrepancy{Kind: KindStatementTruncated, Details: details}
}

// indexHeader maps required column names to their position.
func indexHeader(header []string) (map[string]int, error) {
	index := make(map[string]int, len(header))
	for i, name := range header {
		// A UTF-8 BOM is not whitespace, so TrimSpace leaves it attached to the
		// first column name. Spreadsheets export it routinely, and without this
		// the file is rejected for "missing" a column that is plainly present.
		name = strings.TrimPrefix(name, "\ufeff")
		index[strings.ToLower(strings.TrimSpace(name))] = i
	}

	var missing []string
	for _, name := range requiredColumns {
		if _, ok := index[name]; !ok {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("statement is missing required column(s): %s",
			strings.Join(missing, ", "))
	}
	return index, nil
}

func parseRow(line int, record []string, index map[string]int) (StatementRow, error) {
	field := func(name string) string {
		i := index[name]
		if i >= len(record) {
			return ""
		}
		return strings.TrimSpace(record[i])
	}

	row := StatementRow{
		Line:        line,
		ExternalRef: field("external_ref"),
		Currency:    strings.ToUpper(field("currency")),
	}

	postedAt, err := parseTime(field("posted_at"))
	if err != nil {
		return StatementRow{}, fmt.Errorf("posted_at: %w", err)
	}
	row.PostedAt = postedAt

	if row.DebitAccountID, err = uuid.Parse(field("debit_account_id")); err != nil {
		return StatementRow{}, fmt.Errorf("debit_account_id: %w", err)
	}
	if row.CreditAccountID, err = uuid.Parse(field("credit_account_id")); err != nil {
		return StatementRow{}, fmt.Errorf("credit_account_id: %w", err)
	}

	// Amounts are minor units, like everywhere else in this system. A decimal
	// point here means the producer is using a different unit, which would
	// silently misreconcile by a factor of 100 -- so reject it loudly.
	raw := field("amount")
	if strings.ContainsAny(raw, ".,") {
		return StatementRow{}, fmt.Errorf(
			"amount %q must be an integer in minor units, not a decimal", raw)
	}
	if row.Amount, err = strconv.ParseInt(raw, 10, 64); err != nil {
		return StatementRow{}, fmt.Errorf("amount: %w", err)
	}
	if row.Amount <= 0 {
		return StatementRow{}, fmt.Errorf("amount must be positive, got %d", row.Amount)
	}

	if len(row.Currency) != 3 {
		return StatementRow{}, fmt.Errorf("currency %q must be a 3-letter code", row.Currency)
	}
	if row.DebitAccountID == row.CreditAccountID {
		return StatementRow{}, errors.New("debit and credit accounts must differ")
	}

	return row, nil
}

// acceptedTimeLayouts are the formats a statement may use for posted_at, from
// most to least specific. Producers vary; dates alone are common.
var acceptedTimeLayouts = []string{
	time.RFC3339Nano,
	time.RFC3339,
	"2006-01-02T15:04:05",
	"2006-01-02 15:04:05",
	"2006-01-02",
}

func parseTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, errors.New("must not be empty")
	}
	for _, layout := range acceptedTimeLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("%q is not a recognised date or timestamp", s)
}

func unparseable(line int, reason string) Discrepancy {
	return Discrepancy{
		Kind:    KindUnparseableRow,
		Details: map[string]any{"line": line, "reason": reason},
	}
}
