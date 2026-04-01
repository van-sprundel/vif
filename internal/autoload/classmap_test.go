package autoload

import (
	"os"
	"path/filepath"
	"testing"
)

func TestScanClassmapSingleFile(t *testing.T) {
	dir := t.TempDir()
	writePhp(t, dir, "src/Foo.php", `<?php
namespace App\Models;

class Foo {
}
`)

	classmap, err := ScanClassmap(dir, []string{"src/Foo.php"}, nil)
	if err != nil {
		t.Fatalf("ScanClassmap: %v", err)
	}

	want := `App\Models\Foo`
	if path, ok := classmap[want]; !ok {
		t.Errorf("missing %q in classmap: %v", want, classmap)
	} else if path != "src/Foo.php" {
		t.Errorf("classmap[%q] = %q, want %q", want, path, "src/Foo.php")
	}
}

func TestScanClassmapDirectory(t *testing.T) {
	dir := t.TempDir()
	writePhp(t, dir, "lib/Bar.php", `<?php
namespace Lib;

class Bar {}
interface BarInterface {}
`)
	writePhp(t, dir, "lib/Sub/Baz.php", `<?php
namespace Lib\Sub;

trait BazTrait {}
enum Color {}
`)

	classmap, err := ScanClassmap(dir, []string{"lib"}, nil)
	if err != nil {
		t.Fatalf("ScanClassmap: %v", err)
	}

	expected := map[string]string{
		`Lib\Bar`:          "lib/Bar.php",
		`Lib\BarInterface`: "lib/Bar.php",
		`Lib\Sub\BazTrait`: "lib/Sub/Baz.php",
		`Lib\Sub\Color`:    "lib/Sub/Baz.php",
	}

	for fqcn, wantPath := range expected {
		if got, ok := classmap[fqcn]; !ok {
			t.Errorf("missing %q", fqcn)
		} else if got != wantPath {
			t.Errorf("classmap[%q] = %q, want %q", fqcn, got, wantPath)
		}
	}
}

func TestScanClassmapNoNamespace(t *testing.T) {
	dir := t.TempDir()
	writePhp(t, dir, "legacy.php", `<?php
class LegacyClass {}
`)

	classmap, err := ScanClassmap(dir, []string{"legacy.php"}, nil)
	if err != nil {
		t.Fatalf("ScanClassmap: %v", err)
	}

	if _, ok := classmap["LegacyClass"]; !ok {
		t.Errorf("missing LegacyClass in classmap: %v", classmap)
	}
}

func TestScanClassmapAbstractFinal(t *testing.T) {
	dir := t.TempDir()
	writePhp(t, dir, "src/Types.php", `<?php
namespace App;

abstract class AbstractBase {}
final class Concrete {}
`)

	classmap, err := ScanClassmap(dir, []string{"src/Types.php"}, nil)
	if err != nil {
		t.Fatalf("ScanClassmap: %v", err)
	}

	for _, want := range []string{`App\AbstractBase`, `App\Concrete`} {
		if _, ok := classmap[want]; !ok {
			t.Errorf("missing %q in classmap: %v", want, classmap)
		}
	}
}

func TestScanClassmapMissingEntry(t *testing.T) {
	dir := t.TempDir()

	// Should not error on missing paths — just skip them.
	classmap, err := ScanClassmap(dir, []string{"nonexistent/dir"}, nil)
	if err != nil {
		t.Fatalf("ScanClassmap: %v", err)
	}
	if len(classmap) != 0 {
		t.Errorf("expected empty classmap, got %v", classmap)
	}
}

func TestScanClassmapSkipsNonPhp(t *testing.T) {
	dir := t.TempDir()
	writePhp(t, dir, "lib/readme.txt", "class NotPhp {}")
	writePhp(t, dir, "lib/Actual.php", "<?php\nclass Actual {}")

	classmap, err := ScanClassmap(dir, []string{"lib"}, nil)
	if err != nil {
		t.Fatalf("ScanClassmap: %v", err)
	}

	if _, ok := classmap["NotPhp"]; ok {
		t.Error("should not have scanned .txt file")
	}
	if _, ok := classmap["Actual"]; !ok {
		t.Error("missing Actual class from .php file")
	}
}

func TestScanClassmapSkipsHeredoc(t *testing.T) {
	dir := t.TempDir()
	// Simulates PHPUnit's Generator.php pattern: trait declarations inside heredoc strings.
	writePhp(t, dir, "src/Generator.php", `<?php
namespace App\Mock;

class Generator {
    private const TRAIT_TPL = <<<'EOT'
namespace App\Mock;

trait MockedCloneMethod
{
    public function __clone(): void
    {
    }
}
EOT;

    public function generate() {}
}
`)

	classmap, err := ScanClassmap(dir, []string{"src/Generator.php"}, nil)
	if err != nil {
		t.Fatalf("ScanClassmap: %v", err)
	}

	// Generator should be found.
	if _, ok := classmap[`App\Mock\Generator`]; !ok {
		t.Error("missing App\\Mock\\Generator in classmap")
	}
	// MockedCloneMethod should NOT be found (it's inside a heredoc).
	if _, ok := classmap[`App\Mock\MockedCloneMethod`]; ok {
		t.Error("MockedCloneMethod should not be in classmap (it's inside a heredoc string)")
	}
}

func TestScanClassmapReadonlyClass(t *testing.T) {
	dir := t.TempDir()
	writePhp(t, dir, "src/Value.php", `<?php
namespace App\DTO;

readonly class Value {
    public function __construct(public string $name) {}
}
`)

	classmap, err := ScanClassmap(dir, []string{"src/Value.php"}, nil)
	if err != nil {
		t.Fatalf("ScanClassmap: %v", err)
	}

	if _, ok := classmap[`App\DTO\Value`]; !ok {
		t.Errorf("missing readonly class App\\DTO\\Value in classmap: %v", classmap)
	}
}

func TestScanClassmapSkipsNowdoc(t *testing.T) {
	dir := t.TempDir()
	writePhp(t, dir, "src/Template.php", `<?php
namespace App;

class Template {
    private $tpl = <<<HEREDOC
namespace Fake;

class FakeClass {}
HEREDOC;
}
`)

	classmap, err := ScanClassmap(dir, []string{"src/Template.php"}, nil)
	if err != nil {
		t.Fatalf("ScanClassmap: %v", err)
	}

	if _, ok := classmap[`App\Template`]; !ok {
		t.Error("missing App\\Template")
	}
	if _, ok := classmap[`Fake\FakeClass`]; ok {
		t.Error("FakeClass should not be in classmap (it's inside a heredoc)")
	}
}

func TestScanClassmapExcludePatterns(t *testing.T) {
	dir := t.TempDir()
	writePhp(t, dir, "src/Foo.php", `<?php
namespace App;

class Foo {}
`)
	// Use "Tests" (capital T) to match the /Tests/ exclusion pattern.
	writePhp(t, dir, "src/Tests/Bar.php", `<?php
namespace App\Tests;

class Bar {}
`)

	classmap, err := ScanClassmap(dir, []string{"src"}, []string{"/Tests/"})
	if err != nil {
		t.Fatalf("ScanClassmap: %v", err)
	}

	if _, ok := classmap[`App\Foo`]; !ok {
		t.Errorf("expected App\\Foo in classmap, got: %v", classmap)
	}
	if _, ok := classmap[`App\Tests\Bar`]; ok {
		t.Error("App\\Tests\\Bar should be excluded by /Tests/ pattern")
	}
}

func TestScanClassmapSkipsAnonymousClass(t *testing.T) {
	dir := t.TempDir()
	writePhp(t, dir, "src/Factory.php", `<?php
namespace App;

class Factory
{
    public function make(): object
    {
        return new class () {
        };
    }
}
`)

	classmap, err := ScanClassmap(dir, []string{"src/Factory.php"}, nil)
	if err != nil {
		t.Fatalf("ScanClassmap: %v", err)
	}

	if _, ok := classmap[`App\Factory`]; !ok {
		t.Fatalf("missing App\\Factory in classmap: %v", classmap)
	}
	if len(classmap) != 1 {
		t.Fatalf("expected only App\\Factory, got %v", classmap)
	}
}

func TestScanClassmapBracketedNamespace(t *testing.T) {
	dir := t.TempDir()
	writePhp(t, dir, "src/Bracketed.php", `<?php
namespace App\Scoped {
    final class Bracketed {}
}
`)

	classmap, err := ScanClassmap(dir, []string{"src/Bracketed.php"}, nil)
	if err != nil {
		t.Fatalf("ScanClassmap: %v", err)
	}

	if _, ok := classmap[`App\Scoped\Bracketed`]; !ok {
		t.Fatalf("missing App\\Scoped\\Bracketed in classmap: %v", classmap)
	}
}

func TestScanClassmapAttributesAndInlineDeclarations(t *testing.T) {
	dir := t.TempDir()
	writePhp(t, dir, "src/Attributed.php", `<?php namespace App\Inline;

#[Example]
final readonly class Attributed {}

interface Contract {}
`)

	classmap, err := ScanClassmap(dir, []string{"src/Attributed.php"}, nil)
	if err != nil {
		t.Fatalf("ScanClassmap: %v", err)
	}

	for _, want := range []string{`App\Inline\Attributed`, `App\Inline\Contract`} {
		if _, ok := classmap[want]; !ok {
			t.Fatalf("missing %s in classmap: %v", want, classmap)
		}
	}
}

func TestScanClassmapSkipsBlockComments(t *testing.T) {
	dir := t.TempDir()
	writePhp(t, dir, "src/Commented.php", `<?php
namespace App;

/*
class FakeCommentClass {}
*/
class RealClass {}
`)

	classmap, err := ScanClassmap(dir, []string{"src/Commented.php"}, nil)
	if err != nil {
		t.Fatalf("ScanClassmap: %v", err)
	}

	if _, ok := classmap[`App\FakeCommentClass`]; ok {
		t.Fatalf("FakeCommentClass should not be discovered from a block comment: %v", classmap)
	}
	if _, ok := classmap[`App\RealClass`]; !ok {
		t.Fatalf("missing App\\RealClass in classmap: %v", classmap)
	}
}

func TestScanClassmapScansHackFiles(t *testing.T) {
	dir := t.TempDir()
	writePhp(t, dir, "src/Hack.hh", `<?hh
namespace App\Hack;

trait UsesHack {}
`)

	classmap, err := ScanClassmap(dir, []string{"src"}, nil)
	if err != nil {
		t.Fatalf("ScanClassmap: %v", err)
	}

	if _, ok := classmap[`App\Hack\UsesHack`]; !ok {
		t.Fatalf("missing App\\Hack\\UsesHack in classmap: %v", classmap)
	}
}

func writePhp(t *testing.T, base, rel, content string) {
	t.Helper()
	path := filepath.Join(base, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}
