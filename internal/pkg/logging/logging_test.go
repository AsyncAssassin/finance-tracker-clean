package logging

import (
	"bytes"
	"strings"
	"testing"
)

func TestNewLevelsAndFormats(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	log, err := New(&buf, "info", "json")
	if err != nil {
		t.Fatal(err)
	}
	log.Debug("hidden")
	log.Info("shown", "k", "v")
	if out := buf.String(); strings.Contains(out, "hidden") || !strings.Contains(out, `"msg":"shown"`) || !strings.Contains(out, `"k":"v"`) {
		t.Errorf("json output = %q", out)
	}

	buf.Reset()
	log, err = New(&buf, "DEBUG", "text")
	if err != nil {
		t.Fatal(err)
	}
	log.Debug("visible")
	if out := buf.String(); !strings.Contains(out, "level=DEBUG") || !strings.Contains(out, "msg=visible") {
		t.Errorf("text output = %q", out)
	}

	for _, lvl := range []string{"warn", "warning", "error", ""} {
		if _, err := New(&buf, lvl, ""); err != nil {
			t.Errorf("level %q rejected: %v", lvl, err)
		}
	}
	if _, err := New(&buf, "loud", "json"); err == nil {
		t.Error("bad level accepted")
	}
	if _, err := New(&buf, "info", "xml"); err == nil {
		t.Error("bad format accepted")
	}
}
