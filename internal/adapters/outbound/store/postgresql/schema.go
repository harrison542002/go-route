package postgresql

import _ "embed"

//go:embed schema.sql
var Schema string
