package http

import (
	"io/fs"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// The pages are looked at on an old iPad that stopped at iOS 9 (Safari 9). Nothing here can run that, so what it does not have is looked for
// in what is served, and the scripts, which have to run there, are tested in node.

// The automations script is tested in node, with fakes for the document and the request. Skipped where node isn't installed.
func TestAutomationsScript(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node isn't installed")
	}

	out, err := exec.Command(node, "js_test/automations_test.js").CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}

func stylesheets(t *testing.T) map[string]string {
	t.Helper()

	sheets := map[string]string{}

	err := fs.WalkDir(contentFS, "content/static/css", func(name string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}

		data, readErr := contentFS.ReadFile(name)
		sheets[name] = regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAllString(string(data), "")

		return readErr
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(sheets) == 0 {
		t.Fatal("no stylesheets")
	}

	return sheets
}

// Gap in a flex container needs Safari 14.1: an old iPad draws the items touching, so spacing is by margins.
func TestStylesheets_UseNoGapInFlexContainers(t *testing.T) {
	gap := regexp.MustCompile(`(^|[;{\s])(row-|column-)?gap\s*:`)

	for name, css := range stylesheets(t) {
		if gap.MatchString(css) {
			t.Errorf("%s has a gap: use margins on the items (see the end of the file)", name)
		}
	}
}

// overflow-wrap: anywhere is for modern browsers, and is not understood by an old Safari, which would then let a long word run out of its card.
func TestStylesheets_WrapLongWordsWhereOverflowWrapAnywhereIsNotKnown(t *testing.T) {
	for name, css := range stylesheets(t) {
		for _, block := range strings.Split(css, "}") {
			if strings.Contains(block, "overflow-wrap: anywhere") && !strings.Contains(block, "word-wrap: break-word") {
				t.Errorf("%s: %q has overflow-wrap: anywhere and no word-wrap before it", name, strings.TrimSpace(block))
			}
		}
	}
}

// The colours of the new syntax (hsl(210 50% 50% / 0.5)) are not understood by Safari 9, which drops the whole declaration.
func TestStylesheets_UseTheColourSyntaxWithCommas(t *testing.T) {
	space := regexp.MustCompile(`(hsla?|rgba?)\([^),]*\s[^),]*\)`)

	for name, css := range stylesheets(t) {
		if m := space.FindString(css); m != "" {
			t.Errorf("%s: %q: use commas", name, m)
		}
	}
}
