//go:build !linux && !darwin

package installationlock

// LuaJIT installation is unavailable on these platforms; preserve ordinary
// game updates without imposing a Unix-only dependency.
func exclusive(string) (func(), error) { return func() {}, nil }
