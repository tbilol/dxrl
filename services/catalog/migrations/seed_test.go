package migrations

import (
	"io/fs"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"unicode"

	commonv1 "github.com/tbilol/dxrl/gen/go/common/v1"
)

// seedRow is one product_types row inserted by a migration.
type seedRow struct {
	id               int32
	key, uz, ru, cat string
	sortOrder        int
	file             string
}

// rowPattern matches a VALUES tuple of `INSERT INTO product_types (id, key,
// name_uz, name_ru, category, sort_order)`.
var rowPattern = regexp.MustCompile(`\(\s*(\d+),\s*'((?:[^']|'')*)',\s*'((?:[^']|'')*)',\s*'((?:[^']|'')*)',\s*'([A-Z_]+)',\s*(\d+)\s*\)`)

// seedRows parses every product_types row the Up sections of the embedded
// migrations insert.
func seedRows(t *testing.T) []seedRow {
	t.Helper()
	files, err := fs.Glob(FS, "*.sql")
	if err != nil {
		t.Fatal(err)
	}
	var rows []seedRow
	for _, name := range files {
		b, err := fs.ReadFile(FS, name)
		if err != nil {
			t.Fatal(err)
		}
		up, _, _ := strings.Cut(string(b), "-- +goose Down")
		for _, stmt := range strings.Split(up, ";") {
			if !strings.Contains(stmt, "INSERT INTO product_types") {
				continue
			}
			for _, m := range rowPattern.FindAllStringSubmatch(stmt, -1) {
				id, _ := strconv.Atoi(m[1])
				order, _ := strconv.Atoi(m[6])
				unq := func(s string) string { return strings.ReplaceAll(s, "''", "'") }
				rows = append(rows, seedRow{id: int32(id), key: unq(m[2]), uz: unq(m[3]), ru: unq(m[4]), cat: m[5],
					sortOrder: order, file: name})
			}
		}
	}
	return rows
}

// TestSeedCoversEveryProductTypeOnce: the migrations insert exactly one row
// per defined non-zero common.v1.ProductType value (id = the number) and
// nothing else, with the key product.proto prescribes, a defined category,
// uz (Latin) and ru (Cyrillic) names and distinct sort orders.
func TestSeedCoversEveryProductTypeOnce(t *testing.T) {
	rows := seedRows(t)
	if len(rows) == 0 {
		t.Fatal("no product_types rows found in the migrations")
	}
	seen := map[int32]string{}
	orders := map[int]int32{}
	for _, r := range rows {
		name, ok := commonv1.ProductType_name[r.id]
		if !ok || r.id == 0 {
			t.Errorf("%s: row %d is not a defined ProductType value", r.file, r.id)
			continue
		}
		if prev, dup := seen[r.id]; dup {
			t.Errorf("%s: %s seeded twice (also in %s)", r.file, name, prev)
		}
		seen[r.id] = r.file
		if want := strings.ReplaceAll(strings.ToLower(strings.TrimPrefix(name, "PRODUCT_TYPE_")), "_", "-"); r.key != want {
			t.Errorf("%s: key %q, want %q (product.proto rule)", name, r.key, want)
		}
		if v, ok := commonv1.ProductCategory_value["PRODUCT_CATEGORY_"+r.cat]; !ok || v == 0 {
			t.Errorf("%s: category %q is not a defined ProductCategory", name, r.cat)
		}
		if r.uz == "" || hasScript(r.uz, unicode.Cyrillic) {
			t.Errorf("%s: uz name %q must be non-empty Latin text", name, r.uz)
		}
		if !hasScript(r.ru, unicode.Cyrillic) {
			t.Errorf("%s: ru name %q must be Cyrillic", name, r.ru)
		}
		if other, dup := orders[r.sortOrder]; dup {
			t.Errorf("%s: sort_order %d already used by %s", name, r.sortOrder, commonv1.ProductType(other))
		}
		orders[r.sortOrder] = r.id
	}
	for v, name := range commonv1.ProductType_name {
		if _, ok := seen[v]; v != 0 && !ok {
			t.Errorf("%s (%d) has no product_types row: add it in a new migration", name, v)
		}
	}
}

// TestSeedMatchesProductProto compares names and categories with the enum
// comments and sections of proto/common/v1/product.proto, when the repository
// layout makes it reachable (not in a Docker build context).
func TestSeedMatchesProductProto(t *testing.T) {
	src, err := os.ReadFile("../../../proto/common/v1/product.proto")
	if err != nil {
		t.Skipf("product.proto not reachable: %v", err)
	}
	sections := map[string]string{"Grains": "GRAIN_INDUSTRIAL", "Vegetables": "VEGETABLE", "Legumes": "LEGUME",
		"Melons": "MELON", "Fruit": "FRUIT", "Nuts": "NUT"}
	section := regexp.MustCompile(`^\s*// ---- (\w+)`)
	label := regexp.MustCompile(`^\s*// ([a-z-]+): (.+?) / (.+?) \(.+\)\.$`)
	value := regexp.MustCompile(`^\s*PRODUCT_TYPE_[A-Z_]+ = (\d+);`)
	type ref struct{ key, uz, ru, cat string }
	want := map[int32]ref{}
	var cat string
	var last []string
	for _, line := range strings.Split(string(src), "\n") {
		if strings.HasPrefix(line, "enum ProductCategory") {
			break
		}
		if m := section.FindStringSubmatch(line); m != nil {
			cat = sections[m[1]]
		} else if m := label.FindStringSubmatch(line); m != nil {
			last = m[1:]
		} else if m := value.FindStringSubmatch(line); m != nil && m[1] != "0" && last != nil {
			n, _ := strconv.Atoi(m[1])
			want[int32(n)] = ref{key: last[0], uz: last[1], ru: last[2], cat: cat}
			last = nil
		}
	}
	if len(want) == 0 {
		t.Fatal("parsed no product labels from product.proto")
	}
	for _, r := range seedRows(t) {
		w, ok := want[r.id]
		if !ok {
			t.Errorf("row %d has no labelled value in product.proto", r.id)
			continue
		}
		if got := (ref{r.key, r.uz, r.ru, r.cat}); got != w {
			t.Errorf("row %d = %+v, product.proto says %+v", r.id, got, w)
		}
	}
}

func hasScript(s string, script *unicode.RangeTable) bool {
	for _, r := range s {
		if unicode.Is(script, r) {
			return true
		}
	}
	return false
}
