// Package installationmaintenance serializes maintenance and lifecycle writes
// against both registered identities and physical installation directories.
package installationmaintenance

import (
	"context"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"strings"
	"sync"

	"github.com/google/uuid"
)

var ErrBusy = errors.New("DST installation is under maintenance")

type Resource struct{ TargetID, InstallationID, Directory string }

func (r Resource) Keys() []string {
	keys := []string{r.TargetID + "\x00id:" + r.InstallationID}
	if directory := Directory(r.TargetID, r.Directory); directory != "" {
		keys = append(keys, r.TargetID+"\x00path:"+directory)
	}
	return keys
}
func Directory(target, directory string) string {
	if strings.TrimSpace(directory) == "" {
		return ""
	}
	if target == "local" {
		if resolved, err := filepath.EvalSymlinks(directory); err == nil {
			directory = resolved
		}
	}
	directory = path.Clean(strings.ReplaceAll(directory, "\\", "/"))
	// Windows paths are case insensitive; never case-fold Unix directories.
	if len(directory) > 1 && directory[1] == ':' {
		directory = strings.ToLower(directory)
	}
	for _, suffix := range []string{"/bin64/dontstarve_dedicated_server_nullrenderer_x64.exe", "/bin/dontstarve_dedicated_server_nullrenderer.exe", "/bin64/dontstarve_dedicated_server_nullrenderer_x64", "/bin/dontstarve_dedicated_server_nullrenderer", "/bin64", "/bin"} {
		directory = strings.TrimSuffix(directory, suffix)
	}
	return directory
}
func Overlaps(a, b Resource) bool {
	for _, first := range a.Keys() {
		for _, second := range b.Keys() {
			if first == second {
				return true
			}
		}
	}
	return false
}

type BusyError struct{ Owner string }

func (e *BusyError) Error() string { return fmt.Sprintf("%s: %s", ErrBusy, e.Owner) }
func (*BusyError) Unwrap() error   { return ErrBusy }

type entry struct {
	owner string
	refs  int
}
type ownershipKey struct{}
type Gate struct {
	mu      sync.Mutex
	owners  map[string]entry
	pending func(context.Context, []Resource, string) error
}

func New(pending func(context.Context, []Resource, string) error) *Gate {
	return &Gate{owners: make(map[string]entry), pending: pending}
}
func Owner(ctx context.Context) string { value, _ := ctx.Value(ownershipKey{}).(string); return value }
func (g *Gate) Acquire(ctx context.Context, resources []Resource, owner string) (context.Context, func(), error) {
	if g == nil {
		return ctx, func() {}, nil
	}
	if err := ctx.Err(); err != nil {
		return ctx, nil, err
	}
	inherited := Owner(ctx)
	if owner == "" {
		owner = inherited
	}
	if owner == "" {
		owner = "operation:" + uuid.NewString()
	}
	if inherited != "" && inherited != owner {
		return ctx, nil, ErrBusy
	}
	keys := make(map[string]bool)
	for _, resource := range resources {
		for _, key := range resource.Keys() {
			keys[key] = true
		}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	for key := range keys {
		if current := g.owners[key]; current.refs > 0 && current.owner != owner {
			return ctx, nil, &BusyError{Owner: current.owner}
		}
	}
	if g.pending != nil {
		if err := g.pending(ctx, resources, owner); err != nil {
			return ctx, nil, err
		}
	}
	for key := range keys {
		current := g.owners[key]
		current.owner, current.refs = owner, current.refs+1
		g.owners[key] = current
	}
	var once sync.Once
	return context.WithValue(ctx, ownershipKey{}, owner), func() {
		once.Do(func() {
			g.mu.Lock()
			defer g.mu.Unlock()
			for key := range keys {
				current := g.owners[key]
				current.refs--
				if current.refs == 0 {
					delete(g.owners, key)
				} else {
					g.owners[key] = current
				}
			}
		})
	}, nil
}
