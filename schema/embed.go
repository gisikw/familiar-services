// Package schema embeds the canonical database schema.
package schema

import _ "embed"

// Continuity is the canonical continuity database schema.
//
//go:embed continuity.sql
var Continuity string
