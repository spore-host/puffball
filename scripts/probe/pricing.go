package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/pricing"
)

// priceListRegion is where the Price List API is served. It is the API's own
// endpoint, not a Braket device region.
const priceListRegion = "us-east-1"

// runPricing is G0(b) (#3): does the Price List API carry Braket prices?
// Read-only; the Price List API is not billed.
func runPricing(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("pricing", flag.ContinueOnError)
	out := fs.String("out", ".probe/pricing", "output directory")
	code := fs.String("service-code", "AmazonBraket", "Price List service code to query")
	if err := fs.Parse(args); err != nil {
		return usageErr{err.Error()}
	}
	cfg, rec, err := loadConfig(ctx, priceListRegion)
	if err != nil {
		return err
	}
	client := pricing.NewFromConfig(cfg)

	// Every service code, to find any Braket-like code rather than assume one.
	var codes []string
	sp := pricing.NewDescribeServicesPaginator(client, &pricing.DescribeServicesInput{})
	for i := 0; sp.HasMorePages(); i++ {
		page, err := sp.NextPage(ctx)
		if err != nil {
			return fmt.Errorf("DescribeServices: %w", err)
		}
		for _, s := range page.Services {
			c := aws.ToString(s.ServiceCode)
			if strings.Contains(strings.ToLower(c), "braket") {
				codes = append(codes, c)
			}
		}
	}
	if err := writeJSON(filepath.Join(*out, "braket-service-codes.json"), codes); err != nil {
		return err
	}

	ds, err := client.DescribeServices(ctx, &pricing.DescribeServicesInput{ServiceCode: code})
	if err != nil {
		return fmt.Errorf("DescribeServices(%s): %w", *code, err)
	}
	if err := writeRaw(filepath.Join(*out, "describe-services.json"), rec.Last()); err != nil {
		return err
	}
	_ = ds

	n := 0
	gp := pricing.NewGetProductsPaginator(client, &pricing.GetProductsInput{ServiceCode: code, MaxResults: aws.Int32(100)})
	for i := 0; gp.HasMorePages(); i++ {
		page, err := gp.NextPage(ctx)
		if err != nil {
			return fmt.Errorf("GetProducts(%s): %w", *code, err)
		}
		n += len(page.PriceList)
		if err := writeRaw(filepath.Join(*out, fmt.Sprintf("get-products.%d.json", i)), rec.Last()); err != nil {
			return err
		}
	}
	fmt.Fprintf(os.Stderr, "service codes containing 'braket': %v; %s: %d products saved to %s\n", codes, *code, n, *out)
	return nil
}
