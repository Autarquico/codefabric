//go:build integration && race

package main

import "time"

// holdLimit is how long a transaction of the load may hold the writer without
// counting as slow: -race multiplies the cost of a transaction.
const holdLimit = 500 * time.Millisecond
