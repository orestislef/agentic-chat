// Package version holds the build version, set through -ldflags at release time.
package version

// Version is overwritten by the Docker build.
var Version = "dev"
