package price

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/pricing"
)

// Product is one on-demand price from the AWS Price List.
type Product struct {
	UsageType       string
	Unit            string
	USD             string
	PublicationDate string
}

// ParseProducts reads GetProducts PriceList entries (each a JSON document as
// a string). Only on-demand terms are kept.
func ParseProducts(entries []string) ([]Product, error) {
	var out []Product
	for i, s := range entries {
		var p struct {
			Product struct {
				Attributes struct {
					UsageType string `json:"usagetype"`
				} `json:"attributes"`
			} `json:"product"`
			Terms struct {
				OnDemand map[string]struct {
					PriceDimensions map[string]struct {
						Unit         string            `json:"unit"`
						PricePerUnit map[string]string `json:"pricePerUnit"`
					} `json:"priceDimensions"`
				} `json:"OnDemand"`
			} `json:"terms"`
			PublicationDate string `json:"publicationDate"`
		}
		if err := json.Unmarshal([]byte(s), &p); err != nil {
			return nil, fmt.Errorf("price list entry %d: %w", i, err)
		}
		for _, term := range p.Terms.OnDemand {
			for _, dim := range term.PriceDimensions {
				out = append(out, Product{UsageType: p.Product.Attributes.UsageType, Unit: dim.Unit, USD: dim.PricePerUnit["USD"], PublicationDate: p.PublicationDate})
			}
		}
	}
	return out, nil
}

// ProductsAPI is the Price List call the drift check uses. The Price List API
// is not billed.
type ProductsAPI interface {
	GetProducts(ctx context.Context, in *pricing.GetProductsInput, opts ...func(*pricing.Options)) (*pricing.GetProductsOutput, error)
}

// ServiceCode is the Price List code for Amazon Braket (measured, #3).
const ServiceCode = "AmazonBraket"

// FetchProducts reads every AmazonBraket product from the Price List.
func FetchProducts(ctx context.Context, c ProductsAPI) ([]Product, error) {
	var entries []string
	pg := pricing.NewGetProductsPaginator(c, &pricing.GetProductsInput{ServiceCode: aws.String(ServiceCode), MaxResults: aws.Int32(100)})
	for pg.HasMorePages() {
		out, err := pg.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("price list GetProducts: %w", err)
		}
		entries = append(entries, out.PriceList...)
	}
	return ParseProducts(entries)
}

// Diff is one disagreement between the table and the Price List.
type Diff struct {
	Device    string `json:"device"`
	UsageType string `json:"usage_type"`
	Table     string `json:"table_usd"`
	PriceList string `json:"price_list_usd"` // "missing" if not listed
}

func (d Diff) String() string {
	return fmt.Sprintf("%s %s: table %s, Price List %s", d.Device, d.UsageType, d.Table, d.PriceList)
}

// Drift compares the table with Price List products. A device's per-task
// and per-shot usagetypes must be listed at the table's price, with the
// matching unit; a simulator's minutes usagetype must be listed for every
// region it runs in, at the table's price.
func (t *Table) Drift(products []Product) ([]Diff, error) {
	byUsage := map[string][]Product{}
	for _, p := range products {
		byUsage[p.UsageType] = append(byUsage[p.UsageType], p)
	}
	var diffs []Diff
	check := func(dev, usage, unit string, want Nano, matches []Product) error {
		if len(matches) == 0 {
			diffs = append(diffs, Diff{Device: dev, UsageType: usage, Table: want.String(), PriceList: "missing"})
			return nil
		}
		for _, p := range matches {
			got, err := ParseUSD(p.USD)
			if err != nil {
				return fmt.Errorf("price list %s: %w", p.UsageType, err)
			}
			if got != want || p.Unit != unit {
				diffs = append(diffs, Diff{Device: dev, UsageType: p.UsageType, Table: want.String() + " per " + unit, PriceList: got.String() + " per " + p.Unit})
			}
		}
		return nil
	}
	for _, d := range t.Devices {
		if d.Simulator() {
			// Each listed region separately: one delisted region is a diff.
			listed := map[string]bool{}
			for _, u := range d.Usage.Minutes {
				listed[u] = true
				if err := check(d.Alias, u, "minutes", d.perMinute, byUsage[u]); err != nil {
					return nil, err
				}
			}
			// And any region the Price List has that the table doesn't: the
			// region set is never fixed (law 3).
			_, suffix, _ := strings.Cut(d.Usage.Minutes[0], "-")
			var extra []string
			for u := range byUsage {
				if _, s, _ := strings.Cut(u, "-"); s == suffix && !listed[u] {
					extra = append(extra, u)
				}
			}
			sort.Strings(extra)
			for _, u := range extra {
				diffs = append(diffs, Diff{Device: d.Alias, UsageType: u, Table: "not in the table", PriceList: byUsage[u][0].USD + " per " + byUsage[u][0].Unit})
			}
			continue
		}
		if err := check(d.Alias, d.Usage.Task, "Quantum-Task", t.perTask, byUsage[d.Usage.Task]); err != nil {
			return nil, err
		}
		if err := check(d.Alias, d.Usage.Shot, "Quantum-Shot", d.perShot, byUsage[d.Usage.Shot]); err != nil {
			return nil, err
		}
	}
	return diffs, nil
}
