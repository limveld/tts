package main

import "strings"

// quoteQualified builds a double-quoted schema.table identifier.
//
// Partition names cannot be bound as parameters — DDL takes identifiers, not
// values — so they get interpolated into SQL, and interpolation without quoting
// is how injection happens. Every name here is either a constant or read back
// out of pg_class, so the practical risk is nil; the quoting is here so that
// stays true when someone later threads a name in from a flag.
//
// Doubling embedded quotes is the whole of Postgres's escaping rule for
// identifiers.
func quoteQualified(schema, name string) string {
	return quoteIdent(schema) + "." + quoteIdent(name)
}

func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}
