//go:build race

package proxy

// raceDetector reports whether the test binary was built with -race.
// Race-instrumented binaries run the hot loop several times slower, so
// throughput measurements and soak durations are scaled down when set.
const raceDetector = true
