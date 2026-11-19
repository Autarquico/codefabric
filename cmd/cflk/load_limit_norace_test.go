//go:build integration && !race

package main

import "time"

// holdLimit is how long a transaction of the load may hold the writer without counting as slow.
const holdLimit = 250 * time.Millisecond
