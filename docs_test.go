package agentsafe

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The documents cite tests as proof ("this is proven by TestX"). A cited test that was renamed or deleted
// would leave a claim with no proof behind it, so every citation must name a test that exists.
func TestDocumentsCiteTestsThatExist(t *testing.T) {
	defined := map[string]bool{}
	decl := regexp.MustCompile(`(?m)^func ((?:Test|Fuzz)\w+)\(`)
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, "_test.go") {
			return err
		}
		b, err := os.ReadFile(path)
		for _, m := range decl.FindAllStringSubmatch(string(b), -1) {
			defined[m[1]] = true
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	cite := regexp.MustCompile("`((?:Test|Fuzz)\\w+)`")
	docs, _ := filepath.Glob("docs/*.md")
	docs = append(docs, "README.md", "SECURITY.md", "SCORECARD.md")
	cited := 0
	for _, doc := range docs {
		b, err := os.ReadFile(doc)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range cite.FindAllStringSubmatch(string(b), -1) {
			cited++
			if !defined[m[1]] {
				t.Errorf("%s cites %s, which doesn't exist", doc, m[1])
			}
		}
	}
	if cited < 50 {
		t.Fatalf("only %d citations found: is the pattern still right?", cited)
	}
}
