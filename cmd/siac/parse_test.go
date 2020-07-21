package main

import (
	"math"
	"math/big"
	"strings"
	"testing"

	"gitlab.com/NebulousLabs/Sia/types"
	"gitlab.com/NebulousLabs/fastrand"
)

// TestParseFileSize probes the parseFilesize function
func TestParseFilesize(t *testing.T) {
	tests := []struct {
		in, out string
		err     error
	}{
		{"1b", "1", nil},
		{"1 b", "1", nil},
		{"1KB", "1000", nil},
		{"1   kb", "1000", nil},
		{"1 kB", "1000", nil},
		{" 1Kb ", "1000", nil},
		{"1MB", "1000000", nil},
		{"1 MB", "1000000", nil},
		{"   1GB ", "1000000000", nil},
		{"1 GB   ", "1000000000", nil},
		{"1TB", "1000000000000", nil},
		{"1 TB", "1000000000000", nil},
		{"1KiB", "1024", nil},
		{"1 KiB", "1024", nil},
		{"1MiB", "1048576", nil},
		{"1 MiB", "1048576", nil},
		{"1GiB", "1073741824", nil},
		{"1 GiB", "1073741824", nil},
		{"1TiB", "1099511627776", nil},
		{"1 TiB", "1099511627776", nil},
		{"", "", ErrParseSizeUnits},
		{"123", "", ErrParseSizeUnits},
		{"123b", "123", nil},
		{"123 TB", "123000000000000", nil},
		{"123GiB", "132070244352", nil},
		{"123BiB", "", ErrParseSizeAmount},
		{"GB", "", ErrParseSizeAmount},
		{"123G", "", ErrParseSizeUnits},
		{"123B99", "", ErrParseSizeUnits},
		{"12A3456", "", ErrParseSizeUnits},
		{"1.23KB", "1230", nil},
		{"1.234 KB", "1234", nil},
		{"1.2345KB", "1234", nil},
	}
	for _, test := range tests {
		res, err := parseFilesize(test.in)
		if res != test.out || err != test.err {
			t.Errorf("parseFilesize(%v): expected %v %v, got %v %v", test.in, test.out, test.err, res, err)
		}
	}
}

// TestParsePeriod probes the parsePeriod function
func TestParsePeriod(t *testing.T) {
	tests := []struct {
		in, out string
		err     error
	}{
		{"x", "", ErrParsePeriodUnits},
		{"1", "", ErrParsePeriodUnits},
		{"b", "", ErrParsePeriodAmount},
		{"1b", "1", nil},
		{"1 b", "1", nil},
		{"1block", "1", nil},
		{"1 block ", "1", nil},
		{"1blocks", "1", nil},
		{"1 blocks", "1", nil},
		{" 2b ", "2", nil},
		{"2 b", "2", nil},
		{"2block", "2", nil},
		{"2 block", "2", nil},
		{"2blocks", "2", nil},
		{"2 blocks", "2", nil},
		{"2h", "12", nil},
		{"2 h", "12", nil},
		{"2hour", "12", nil},
		{"2 hour", "12", nil},
		{" 2hours ", "12", nil},
		{"2 hours", "12", nil},
		{"0.5d", "72", nil},
		{" 0.5 d", "72", nil},
		{"0.5day", "72", nil},
		{"0.5 day", "72", nil},
		{"0.5days", "72", nil},
		{"0.5 days", "72", nil},
		{"10w", "10080", nil},
		{"10 w", "10080", nil},
		{"10week", "10080", nil},
		{"10 week", "10080", nil},
		{"10weeks", "10080", nil},
		{"10 weeks", "10080", nil},
		{"1 fortnight", "", ErrParsePeriodUnits},
		{"three h", "", ErrParsePeriodAmount},
	}
	for _, test := range tests {
		res, err := parsePeriod(test.in)
		if res != test.out || err != test.err {
			t.Errorf("parsePeriod(%v): expected %v %v, got %v %v", test.in, test.out, test.err, res, err)
		}
	}
}

// TestParseCurrency probes the parseCurrency function.
func TestParseCurrency(t *testing.T) {
	tests := []struct {
		in, out string
		err     error
	}{
		{"x", "", ErrParseCurrencyUnits},
		{"1", "", ErrParseCurrencyUnits},
		{"pS", "", ErrParseCurrencyAmount},
		{"1pS", "1000000000000", nil},
		{"1 pS", "1000000000000", nil},
		{"2nS ", "2000000000000000", nil},
		{"2 nS", "2000000000000000", nil},
		{"0uS", "0", nil},
		{"0 uS", "0", nil},
		{"10mS", "10000000000000000000000", nil},
		{"10 mS", "10000000000000000000000", nil},
		{"2SC", "2000000000000000000000000", nil},
		{"2 SC", "2000000000000000000000000", nil},
		{" 1KS ", "1000000000000000000000000000", nil},
		{"1 KS", "1000000000000000000000000000", nil},
		{"4MS", "4000000000000000000000000000000", nil},
		{"4 MS", "4000000000000000000000000000000", nil},
		{"2GS", "2000000000000000000000000000000000", nil},
		{" 2 GS ", "2000000000000000000000000000000000", nil},
		{"1TS", "1000000000000000000000000000000000000", nil},
		{"1 TS", "1000000000000000000000000000000000000", nil},
		{"0.5TS", "500000000000000000000000000000000000", nil},
		{"0.5 TS", "500000000000000000000000000000000000", nil},
		{"x SC", "", ErrParseCurrencyAmount},
	}
	for _, test := range tests {
		res, err := parseCurrency(test.in)
		if res != test.out || err != test.err {
			t.Errorf("parseCurrency(%v): expected %v %v, got %v %v", test.in, test.out, test.err, res, err)
		}
	}
}

// TestCurrencyUnits probes the currencyUnits function
func TestCurrencyUnits(t *testing.T) {
	tests := []struct {
		in, out string
	}{
		{"1", "1 H"},
		{"1000", "1000 H"},
		{"100000000000", "100000000000 H"},
		{"1000000000000", "1 pS"},
		{"1234560000000", "1.235 pS"},
		{"12345600000000", "12.35 pS"},
		{"123456000000000", "123.5 pS"},
		{"1000000000000000", "1 nS"},
		{"1000000000000000000", "1 uS"},
		{"1000000000000000000000", "1 mS"},
		{"1000000000000000000000000", "1 SC"},
		{"1000000000000000000000000000", "1 KS"},
		{"1000000000000000000000000000000", "1 MS"},
		{"1000000000000000000000000000000000", "1 GS"},
		{"1000000000000000000000000000000000000", "1 TS"},
		{"1234560000000000000000000000000000000", "1.235 TS"},
		{"1234560000000000000000000000000000000000", "1235 TS"},
	}
	for _, test := range tests {
		i, _ := new(big.Int).SetString(test.in, 10)
		out := currencyUnits(types.NewCurrency(i))
		if out != test.out {
			t.Errorf("currencyUnits(%v): expected %v, got %v", test.in, test.out, out)
		}
	}
}

// TestRateLimitUnits probes the ratelimitUnits function
func TestRatelimitUnits(t *testing.T) {
	tests := []struct {
		in  int64
		out string
	}{
		{0, "0 B/s"},
		{123, "123 B/s"},
		{1234, "1.234 KB/s"},
		{1234000, "1.234 MB/s"},
		{1234000000, "1.234 GB/s"},
		{1234000000000, "1.234 TB/s"},
	}
	for _, test := range tests {
		out := ratelimitUnits(test.in)
		if out != test.out {
			t.Errorf("ratelimitUnits(%v): expected %v, got %v", test.in, test.out, out)
		}
	}
}

// TestParseRateLimit probes the parseRatelimit function
func TestParseRatelimit(t *testing.T) {
	tests := []struct {
		in  string
		out int64
		err error
	}{
		{"x", 0, ErrParseRateLimitUnits},
		{"1", 0, ErrParseRateLimitUnits},
		{"B/s", 0, ErrParseRateLimitNoAmount},
		{"Bps", 0, ErrParseRateLimitNoAmount},
		{"1Bps", 0, ErrParseRateLimitAmount},
		{" 1B/s ", 1, nil},
		{"1 B/s", 1, nil},
		{"8Bps", 1, nil},
		{"8 Bps", 1, nil},
		{" 1KB/s ", 1000, nil},
		{"1 KB/s", 1000, nil},
		{"8Kbps", 1000, nil},
		{" 8 Kbps", 1000, nil},
		{"1MB/s", 1000000, nil},
		{"1 MB/s", 1000000, nil},
		{"8Mbps", 1000000, nil},
		{"8 Mbps", 1000000, nil},
		{"1GB/s", 1000000000, nil},
		{"1 GB/s", 1000000000, nil},
		{"8Gbps", 1000000000, nil},
		{"8 Gbps", 1000000000, nil},
		{"1TB/s", 1000000000000, nil},
		{"1 TB/s", 1000000000000, nil},
		{"8Tbps", 1000000000000, nil},
		{"8 Tbps", 1000000000000, nil},
	}

	for _, test := range tests {
		res, err := parseRatelimit(test.in)
		if res != test.out || (err != test.err && !strings.Contains(err.Error(), test.err.Error())) {
			t.Errorf("parsePeriod(%v): expected %v %v, got %v %v", test.in, test.out, test.err, res, err)
		}
	}
}

// TestParsePercentages probes the parsePercentages function
func TestParsePercentages(t *testing.T) {
	tests := []struct {
		in  []float64
		out []float64
	}{
		{[]float64{50.0, 50.0}, []float64{50, 50}},
		{[]float64{49.5, 50.5}, []float64{50, 50}},
		{[]float64{33.1, 33.4, 33.5}, []float64{33, 33, 34}},
		{[]float64{63.1, 33.4, 3.5}, []float64{63, 33, 4}},
		{[]float64{0, 0, 100}, []float64{0, 0, 100}},
		{[]float64{100}, []float64{100}},
	}

	// Test set cases to ensure known edge cases are always handled
	for _, test := range tests {
		res := parsePercentages(test.in)
		for i, v := range res {
			if v != test.out[i] {
				t.Log("Result", res)
				t.Log("Expected", test.out)
				t.Fatal("Result not as expected")
			}
		}
	}

	// For test-long test additional random cases
	if testing.Short() {
		t.SkipNow()
	}

	// Test Random Edge Cases
	for i := 0; i < 10; i++ {
		values := parsePercentages(randomPercentages())
		// Since we can't know what the exact output should be, verify that the
		// values add up to 100 and that none of the values have a non zero
		// remainder
		var total float64
		for _, v := range values {
			_, r := math.Modf(v)
			if r != 0 {
				t.Log(values)
				t.Log(v)
				t.Fatal("Found non zero remainder")
			}
			total += v
		}
		if total != float64(100) {
			t.Log(values)
			t.Log(total)
			t.Fatal("Values should add up to 100 but added up to", total)
		}
	}
}

// randomPercentages creates a slice of pseudo random size, up to 500 elements,
// with random elements that add to 100.
//
// NOTE: this function does not explicitly check that all the elements strictly
// add up to 100 due to potential significant digit rounding errors. It was
// common to see the elements add up to 100.00000000000001.
func randomPercentages() []float64 {
	var p []float64

	remainder := float64(100)
	for i := 0; i < 500; i++ {
		n := float64(fastrand.Intn(1000))
		d := float64(fastrand.Intn(100000)) + n
		val := n / d * 100
		if math.IsNaN(val) || remainder < val {
			continue
		}
		remainder -= val
		p = append(p, val)
		if remainder == 0 {
			break
		}
	}

	// Check if we have a remainder to add
	if remainder > 0 {
		p = append(p, remainder)
	}

	return p
}

// TestSizeString probes the sizeString function
func TestSizeString(t *testing.T) {
	tests := []struct {
		in  uint64
		out string
	}{
		{0, "0 B"},
		{123, "123 B"},
		{1234, "1.234 KB"},
		{1234000, "1.234 MB"},
		{1234000000, "1.234 GB"},
		{1234000000000, "1.234 TB"},
		{1234000000000000, "1.234 PB"},
		{1234000000000000000, "1234 PB"},
	}
	for _, test := range tests {
		out := sizeString(test.in)
		if out != test.out {
			t.Errorf("sizeString(%v): expected %v, got %v", test.in, test.out, out)
		}
	}
}
