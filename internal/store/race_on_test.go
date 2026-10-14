//go:build race

package store

// raceEnabled scales slow tests down under the race detector (about 5x slower).
const raceEnabled = true
