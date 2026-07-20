package parser

import "testing"

func FuzzTokenize(f *testing.F) {
	seeds := []string{
		"SELECT * FROM users",
		"WITH recent AS (SELECT 1) SELECT * FROM recent",
		"INSERT INTO t (a) VALUES (1)",
		"/* comment */ SELECT `col` FROM `db`.`tbl`",
		"SELECT 'from users' FROM dual",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, sql string) {
		_ = tokenize(sql)
	})
}

func FuzzExtractTables(f *testing.F) {
	seeds := []string{
		"SELECT * FROM users JOIN orders",
		"WITH recent AS (SELECT * FROM base) SELECT * FROM recent JOIN orders",
		"UPDATE t SET a=1",
		"DELETE FROM schema.table WHERE 1",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, sql string) {
		_ = extractTables(sql, "db")
		_ = extractTables(sql, "")
	})
}

func FuzzStripUseStatement(f *testing.F) {
	seeds := []string{
		"use mydb; SELECT 1",
		"USE `x` SELECT 1",
		"SELECT 1",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, sql string) {
		_ = stripUseStatement(sql)
		_ = extractDBFromQuery(sql)
	})
}
