package align

import "testing"

func TestParseBatchMFAJSON(t *testing.T) {
	data := []byte(`{
		"tiers": {
			"words": {"entries": [[0.1, 0.4, "γειά"], [0.5, 0.9, "σου"]]}
		}
	}`)
	alignment, err := parseBatchMFAJSON(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(alignment.Words) != 2 || alignment.Words[0].Text != "γειά" || alignment.Words[1].End != 0.9 {
		t.Fatalf("unexpected alignment: %+v", alignment)
	}
}

func TestParseBatchMFAJSONRequiresWordsTier(t *testing.T) {
	if _, err := parseBatchMFAJSON([]byte(`{"tiers":{}}`)); err == nil {
		t.Fatal("expected a missing words tier error")
	}
}
