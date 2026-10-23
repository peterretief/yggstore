//go:build race

package site

import "time"

// Under the race detector wazero and SQLite run many times slower.
func init() { handlerTime = time.Minute }
