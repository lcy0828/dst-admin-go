package steamupdate

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type runnerFunc func(context.Context, string, []string, io.Writer) error

func (f runnerFunc) Run(ctx context.Context, exe string, args []string, out io.Writer) error {
	return f(ctx, exe, args, out)
}

func TestValidatedUpdateBoundsCorruptRepairAndKeepsValidation(t *testing.T) {
	for _, recover := range []bool{false, true} {
		t.Run(map[bool]string{false: "still-corrupt", true: "repaired"}[recover], func(t *testing.T) {
			calls := 0
			var log bytes.Buffer
			runner := runnerFunc(func(_ context.Context, _ string, args []string, out io.Writer) error {
				calls++
				want := []string{"+force_install_dir", "/game", "+login", "anonymous", "+app_update", "343050", "validate", "+quit"}
				if !reflect.DeepEqual(args, want) {
					t.Fatalf("unsafe command: %v", args)
				}
				if recover && calls == 2 {
					return nil
				}
				_, _ = io.WriteString(out, "Error! App '343050' state is 0x486 after ")
				_, _ = io.WriteString(out, "update job.\n")
				return errors.New("exit status 8")
			})
			err := RunValidated(context.Background(), runner, "steamcmd", "/game", "343050", nil, &log)
			if calls != 2 || (err == nil) != recover {
				t.Fatalf("calls=%d err=%v", calls, err)
			}
			if !strings.Contains(log.String(), "validate=true attempt=2/2") {
				t.Fatal(log.String())
			}
		})
	}
}

func TestValidatedUpdateNeverResendsUncertainOrUnrelatedFailure(t *testing.T) {
	for _, output := range []string{"", "Error! App '343050' state is 0x6 after update job.\n", "Error! App '999' state is 0x486 after update job.\n", "failed to login\n"} {
		calls := 0
		runner := runnerFunc(func(_ context.Context, _ string, _ []string, out io.Writer) error {
			calls++
			_, _ = io.WriteString(out, output)
			return context.DeadlineExceeded
		})
		if err := RunValidated(context.Background(), runner, "steamcmd", "/game", "343050", nil, io.Discard); err == nil || calls != 1 {
			t.Fatalf("calls=%d err=%v", calls, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	runner := runnerFunc(func(_ context.Context, _ string, _ []string, out io.Writer) error {
		calls++
		cancel()
		_, _ = io.WriteString(out, "Error! App '343050' state is 0x486 after update job.\n")
		return ctx.Err()
	})
	if err := RunValidated(ctx, runner, "steamcmd", "/game", "343050", nil, io.Discard); !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
}

func TestManifestCorruptionIsNotACompleteInstallation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "appmanifest_343050.acf")
	for _, flags := range []string{"4", "1158", "6", "broken"} {
		if err := os.WriteFile(path, []byte("\"StateFlags\" \""+flags+"\"\n\"buildid\" \"701\"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		err := IntegrityError(path)
		if (err == nil) != (flags == "4") {
			t.Fatalf("flags=%s err=%v", flags, err)
		}
		if flags == "1158" {
			runner := runnerFunc(func(context.Context, string, []string, io.Writer) error { return nil })
			if err := RunValidated(context.Background(), runner, "steamcmd", "/game", "343050", []string{path}, io.Discard); err == nil {
				t.Fatal("accepted successful exit with corrupt manifest")
			}
		}
	}
}

func TestCorruptTerminalOutputFailsEvenWithZeroExit(t *testing.T) {
	calls := 0
	runner := runnerFunc(func(_ context.Context, _ string, _ []string, out io.Writer) error {
		calls++
		_, _ = io.WriteString(out, "Error! App '343050' state is 0x486 after update job.\n")
		return nil
	})
	if err := RunValidated(context.Background(), runner, "steamcmd", "/game", "343050", nil, io.Discard); err == nil || calls != 2 {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
}
