package batchexecute

import (
	"encoding/json"
	"testing"
)

/*
 * A listing row shorter than the documented shape.
 *
 * The flat row is documented as
 *
 *	[content-id, project-id, media-id, type-code, null, detail, ...]
 *
 * but isAssetRow accepts it from four elements up — its flat branch only asks
 * that row[1], row[2] and row[3] be non-empty strings. So a row of four or five
 * elements is a *valid* asset as far as the shape check is concerned, and it
 * then went on to read the title out of row[5] with no length check.
 *
 * The length is the point, not the title. A missing title is a fine answer, and
 * every other field in the function goes through stringAt, which answers ""
 * past the end. This one index was the only unguarded read, and an unrecovered
 * panic in the parser takes the whole process down — a severe response to a
 * listing that is merely shorter than expected.
 */

func TestParseProjectAssetsToleratesAShortRow(t *testing.T) {
	cases := []struct {
		name string
		row  string
	}{
		// The shortest row isAssetRow will call an asset.
		{"four elements", `["content-a","project-a","media-a","CAE"]`},
		{"five elements", `["content-a","project-a","media-a","CAE",null]`},
		// The documented shape, whose row[5] is present but not a detail array.
		{"six elements", `["content-a","project-a","media-a","CAE",null,null]`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assets := ParseProjectAssets(json.RawMessage("[" + tc.row + "]"))
			if len(assets) != 1 {
				t.Fatalf("parsed %d assets, want 1 — the row is a valid asset shape", len(assets))
			}

			got := assets[0]
			if got.MediaID != "media-a" {
				t.Errorf("media id = %q, want media-a", got.MediaID)
			}
			if got.ContentID != "content-a" {
				t.Errorf("content id = %q, want content-a", got.ContentID)
			}
			if got.TypeCode != AssetTypeOriginal {
				t.Errorf("type code = %q, want %q", got.TypeCode, AssetTypeOriginal)
			}
			// A row with no detail slot carries no title, and "" is the honest
			// answer rather than a panic.
			if got.Title != "" {
				t.Errorf("title = %q, want empty", got.Title)
			}
		})
	}
}

// The guard must not have cost the title on the rows that do carry one. A flat
// row with a detail array at row[5] still reads its title from detail[1].
//
// detail[1] rather than detail[0] is not a guess: the app's own flat row puts a
// [seconds, nanos] timestamp pair at detail[0] and the prompt at detail[1]. The
// existing flat-shape test carries exactly that and asserts the ids — but not
// the title, so this path had no coverage at all before the length check went
// in beside it.
func TestParseProjectAssetsStillReadsATitleFromTheDetailSlot(t *testing.T) {
	assets := ParseProjectAssets(json.RawMessage(
		`[["content-a","project-a","media-a","CAE",null,` +
			`[[1789727860,674285000],"a paper boat drifting on a river",null,null,null,"https://lh3.example/a"]]]`))

	if len(assets) != 1 {
		t.Fatalf("parsed %d assets, want 1", len(assets))
	}
	if assets[0].Title != "a paper boat drifting on a river" {
		t.Errorf("title = %q, want %q", assets[0].Title, "a paper boat drifting on a river")
	}
}
