// Package v8diff pins goant's host-defined Date and Intl behaviour to the V8
// build the robot shipped through 26.7.4. See README.md for why neither
// conformance nor test262 can stand in for this.
package v8diff

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// buildRunner builds the goant CLI into a temp dir. The scripts have to run in
// a child process because the timezone is read from the environment once, when
// the process starts, so a single test binary cannot cover several zones.
func buildRunner(t *testing.T) string {
	t.Helper()
	// .exe on Windows, or exec cannot find what go build just wrote: Go names
	// the output exactly what -o says, and Windows will only run a file with an
	// executable extension. This test had never run on Windows before there was
	// a Windows runner to run it on.
	name := "goant"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	bin := filepath.Join(t.TempDir(), name)
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/goant")
	cmd.Dir = "../.."
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building the runner failed: %v\n%s", err, out)
	}
	return bin
}

// Expectations live at expected/<zone>/<script>.txt, with the zone kept as real
// directories. Encoding it into the filename does not work: zone names contain
// both "/" and "_", so America/New_York and America/New/York would collide.
func TestMatchesV8(t *testing.T) {
	runner := buildRunner(t)

	var cases [][2]string // {zone, script}
	err := filepath.WalkDir("expected", func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".txt") {
			return err
		}
		rel, err := filepath.Rel("expected", path)
		if err != nil {
			return err
		}
		zone := filepath.ToSlash(filepath.Dir(rel))
		script := strings.TrimSuffix(filepath.Base(rel), ".txt") + ".js"
		cases = append(cases, [2]string{zone, script})
		return nil
	})
	if err != nil {
		t.Fatalf("reading expectations: %v", err)
	}
	if len(cases) == 0 {
		t.Fatal("no expectations checked in; see README.md")
	}

	for _, c := range cases {
		zone, script := c[0], c[1]
		t.Run(zone+"/"+script, func(t *testing.T) {
			want, err := os.ReadFile(filepath.Join("expected", zone,
				strings.TrimSuffix(script, ".js")+".txt"))
			if err != nil {
				t.Fatalf("reading expectation: %v", err)
			}
			got, err := runScript(runner, script, zone)
			if err != nil {
				t.Fatalf("running %s under TZ=%s: %v", script, zone, err)
			}
			if got != string(want) {
				t.Errorf("%s under TZ=%s no longer matches V8:\n%s",
					script, zone, firstDiff(string(want), got))
			}
		})
	}
}

// runScript runs one probe under the given zone. The probes build an `out`
// array rather than printing, so that the same file can be fed to the V8 oracle
// (which reports the value of its last expression) and to goant.
func runScript(runner, script, zone string) (string, error) {
	src, err := os.ReadFile(script)
	if err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp("", "v8diff-*.js")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(src, []byte("\nprint(out.join(\"\\n\"));\n")...)); err != nil {
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}

	cmd := exec.Command(runner, tmp.Name())
	cmd.Env = append(os.Environ(), "TZ="+zone, "LANG=C", "LC_ALL=C")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), err
	}
	return string(out), nil
}

// firstDiff reports the first few differing lines, which is far more useful
// here than dumping two thousand matching ones.
func firstDiff(want, got string) string {
	w, g := strings.Split(want, "\n"), strings.Split(got, "\n")
	var b strings.Builder
	shown := 0
	for i := 0; i < max(len(w), len(g)) && shown < 10; i++ {
		wl, gl := "", ""
		if i < len(w) {
			wl = w[i]
		}
		if i < len(g) {
			gl = g[i]
		}
		if wl != gl {
			b.WriteString("  line " + itoa(i+1) + "\n    V8:    " + wl + "\n    goant: " + gl + "\n")
			shown++
		}
	}
	if shown == 0 {
		b.WriteString("  (identical lines; trailing newline differs)\n")
	}
	return b.String()
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var d []byte
	for n > 0 {
		d = append([]byte{byte('0' + n%10)}, d...)
		n /= 10
	}
	return string(d)
}
