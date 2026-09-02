#!/bin/sh
# Creates the database used by integration tests. Runs once, on first boot of
# the postgres volume.
set -e
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" <<-EOSQL
	CREATE DATABASE sumzero_test;
EOSQL
