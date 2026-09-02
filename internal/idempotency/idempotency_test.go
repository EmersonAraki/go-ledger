package idempotency_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/EmersonAraki/go-ledger/internal/idempotency"
)

func fingerprint(t *testing.T, body string) []byte {
	t.Helper()
	fp, err := idempotency.Fingerprint("POST", "/transactions", []byte(body))
	if err != nil {
		t.Fatalf("fingerprint %q: %v", body, err)
	}
	return fp
}

// Requests that mean the same thing must fingerprint the same, so a client that
// serializes its retry slightly differently still gets a replay rather than a
// spurious key-reuse rejection.
func TestFingerprintIgnoresKeyOrderAndWhitespace(t *testing.T) {
	t.Parallel()

	base := fingerprint(t, `{"amount":300,"currency":"BRL"}`)

	for name, body := range map[string]string{
		"reordered keys": `{"currency":"BRL","amount":300}`,
		"whitespace":     "{\n  \"amount\" : 300,\n  \"currency\" : \"BRL\"\n}",
		"both":           "{  \"currency\":  \"BRL\"  ,  \"amount\"  : 300 }",
	} {
		t.Run(name, func(t *testing.T) {
			if got := fingerprint(t, body); !bytes.Equal(got, base) {
				t.Errorf("fingerprint differs for an equivalent request:\n  %q", body)
			}
		})
	}
}

// Regression test for a real bug: canonicalising with json.Unmarshal into `any`
// turns every number into a float64, which cannot represent integers above 2^53
// exactly. Amounts here are int64 minor units, so two genuinely different
// transfers would fingerprint identically -- and the second would be answered
// with the first one's response, silently swallowing a transfer.
//
// 2^53 is 9007199254740992; the next integer is not representable as a float64
// and rounds down to it.
func TestFingerprintDistinguishesAmountsBeyondFloat64Precision(t *testing.T) {
	t.Parallel()

	a := fingerprint(t, `{"amount":9007199254740992,"currency":"BRL"}`)
	b := fingerprint(t, `{"amount":9007199254740993,"currency":"BRL"}`)

	if bytes.Equal(a, b) {
		t.Error("amounts 9007199254740992 and 9007199254740993 fingerprint identically; " +
			"the canonicaliser is rounding through float64")
	}
}

// A different request under the same key must be detectable.
func TestFingerprintDistinguishesDifferentRequests(t *testing.T) {
	t.Parallel()

	base := fingerprint(t, `{"amount":300,"currency":"BRL"}`)

	for name, body := range map[string]string{
		"different amount":   `{"amount":301,"currency":"BRL"}`,
		"different currency": `{"amount":300,"currency":"USD"}`,
		"extra field":        `{"amount":300,"currency":"BRL","description":"x"}`,
	} {
		t.Run(name, func(t *testing.T) {
			if got := fingerprint(t, body); bytes.Equal(got, base) {
				t.Errorf("a different request fingerprinted the same:\n  %q", body)
			}
		})
	}
}

// The method and path are part of the identity, so one key cannot be replayed
// across endpoints.
func TestFingerprintCoversMethodAndPath(t *testing.T) {
	t.Parallel()

	body := []byte(`{"amount":300}`)

	base, err := idempotency.Fingerprint("POST", "/transactions", body)
	if err != nil {
		t.Fatalf("fingerprint: %v", err)
	}
	other, err := idempotency.Fingerprint("POST", "/accounts", body)
	if err != nil {
		t.Fatalf("fingerprint: %v", err)
	}
	if bytes.Equal(base, other) {
		t.Error("the same body on a different path fingerprinted identically")
	}
}

func TestFingerprintRejectsMalformedJSON(t *testing.T) {
	t.Parallel()

	if _, err := idempotency.Fingerprint("POST", "/transactions", []byte("{not json")); err == nil {
		t.Error("expected malformed JSON to be rejected")
	}
}

func TestValidateKey(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		key     string
		wantErr error
	}{
		{"empty", "", idempotency.ErrKeyRequired},
		{"uuid", "9f8c1f4e-0b3a-4a9c-9a1e-2f0a5c7d8e9f", nil},
		{"printable punctuation", "order:1234/retry#2", nil},
		{"at the length limit", strings.Repeat("k", idempotency.MaxKeyLength), nil},
		{"over the length limit", strings.Repeat("k", idempotency.MaxKeyLength+1), idempotency.ErrKeyInvalid},
		{"newline", "abc\ndef", idempotency.ErrKeyInvalid},
		{"null byte", "abc\x00def", idempotency.ErrKeyInvalid},
		{"delete character", "abc\x7fdef", idempotency.ErrKeyInvalid},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := idempotency.ValidateKey(tc.key)
			switch {
			case tc.wantErr == nil && err != nil:
				t.Errorf("ValidateKey(%q) = %v, want nil", tc.key, err)
			case tc.wantErr != nil && err == nil:
				t.Errorf("ValidateKey(%q) = nil, want %v", tc.key, tc.wantErr)
			}
		})
	}
}
