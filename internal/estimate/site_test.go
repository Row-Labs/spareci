package estimate

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// The savings calculator on the website (site/) uses the same prices as
// spareci estimate. TestSitePrices fails when they drift apart; regenerate
// with:
//
//	SPARECI_WRITE_SITE_PRICES=1 go test ./internal/estimate -run TestSitePrices
var sitePrices = filepath.Join("..", "..", "site", "assets", "prices.js")

func sitePricesJS(t *testing.T) []byte {
	t.Helper()
	runners := make([]Runner, 0, len(prices))
	for _, r := range prices {
		runners = append(runners, r)
	}
	sort.Slice(runners, func(i, j int) bool {
		a, b := runners[i], runners[j]
		if a.Larger != b.Larger {
			return !a.Larger
		}
		if a.OS != b.OS {
			return a.OS < b.OS
		}
		if a.PerMinute != b.PerMinute {
			return a.PerMinute < b.PerMinute
		}
		return a.SKU < b.SKU
	})
	data, err := json.MarshalIndent(map[string]any{
		"checked":    PricesChecked,
		"source":     PricesSource,
		"selfHosted": SelfHostedPerMinute,
		"runners":    runners,
	}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	b.WriteString("// Generated from internal/estimate/prices.go by TestSitePrices. Do not edit;\n")
	b.WriteString("// run SPARECI_WRITE_SITE_PRICES=1 go test ./internal/estimate -run TestSitePrices\n")
	b.WriteString("window.SPARECI_PRICES = ")
	b.Write(data)
	b.WriteString(";\n")
	return b.Bytes()
}

func TestSitePrices(t *testing.T) {
	want := sitePricesJS(t)
	if os.Getenv("SPARECI_WRITE_SITE_PRICES") != "" {
		if err := os.WriteFile(sitePrices, want, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	got, err := os.ReadFile(sitePrices)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s is out of date with prices.go; regenerate it:\n\tSPARECI_WRITE_SITE_PRICES=1 go test ./internal/estimate -run TestSitePrices", sitePrices)
	}
}
