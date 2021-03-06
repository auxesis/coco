/*
anomalous_coco_send checks if Coco's send behaviour has changed over a time period.

This check is useful for determining if there has been an increase or decrease
in packets being sent by Coco to a storage node.

anomalous_coco_send uses the Kolmogorov-Smirnov Test to determine if data in a
window is anomalous. You can read more about how the KS test works here:

  http://www.physics.csbsju.edu/stats/KS-test.html

At a high level, the test works like this:

 - Query Visage to get a window of data.
 - Bisect the window into two equal length windows.
 - Sort the data points in each window in ascending numerical order of the value.
 - Find the D-statistic - the maximum vertical deviation between the two series.
 - Test if the D-statistic is greater than the user specified acceptable deviation.

Example usage:

  anomalous_coco_send --host ip-10-101-103-42.ap-southeast-2.compute.internal \
					  --target 10.101.103.119 \
					  --endpoint visage.example.org \
					  --window 5m

Protips:

 - --debug flag will output values derived from the supplied command line
   arguments, including the URL from which data is being fetched.
 - The --window setting specifies how large a window of data should be fetched.
   If you fetch a window of 10m, it will be divided into two 5 minute windows
   when performing the KS test.
 - The --maximum-deviation setting is the main knob you will want to tune. It
   determines how much of a deviation is acceptable for the KS test.
*/
package main

import (
	"fmt"
	"github.com/bulletproofnetworks/coco/ks"
	"github.com/bulletproofnetworks/coco/visage"
	"gopkg.in/alecthomas/kingpin.v1"
	"os"
)

// handleErrors performs global error handling for unhandled errors
// hostd on code from http://blog.denevell.org/golang-panic-recover.html
func handleErrors() {
	if e := recover(); e != nil {
		fmt.Println("UNKNOWN: check error:", e)
		os.Exit(3)
	}
}

var (
	host      = kingpin.Flag("host", "The host to query metrics from").Required().String()
	target    = kingpin.Flag("target", "The storage node to test").Required().String()
	endpoint  = kingpin.Flag("endpoint", "Visage endpoint to query").Required().String()
	deviation = kingpin.Flag("maximum-deviation", "Acceptable deviation for KS test").Default("10.0").Float()
	window    = kingpin.Flag("window", "Window of time to analyse").Default("120s").Duration()
	debug     = kingpin.Flag("debug", "Enable verbose output (default false)").Bool()
)

func main() {
	kingpin.Version("1.0.0")
	kingpin.Parse()

	if *debug {
		fmt.Println("Host:", *host)
		fmt.Println("Target:", *target)
		fmt.Println("Endpoint:", *endpoint)
		fmt.Printf("Maximum deviation: %.1f\n", *deviation)
		fmt.Println("Window:", *window)
		fmt.Println("Debug:", *debug)
	}

	// Global error handling
	defer handleErrors()

	// Fetch a window of metrics
	params := visage.Params{
		Endpoint: *endpoint,
		Host:     *host,
		Plugin:   "curl_json-coco",
		Instance: "operations-send-" + *target + ":25826",
		Ds:       "value",
		Window:   *window,
		Debug:    *debug,
	}
	window, err := visage.Fetch(params)
	if err != nil {
		fmt.Printf("UNKNOWN: Unable to fetch Visage JSON: %s\n", err)
		os.Exit(3)
	}

	if len(window) < 20 {
		fmt.Printf("UNKNOWN: Expected > %d datapoints, got %d. Coco running?\n", 20, len(window))
		os.Exit(3)
	}

	// Bisect the window into two equal length windows.
	window1, window2 := ks.BisectAndSortWindow(window)
	// Find the D-statistic
	max, maxIndex := ks.FindMaxDeviation(window1, window2)

	if *debug {
		fmt.Println("Window 1:")
		fmt.Println(window1)
		fmt.Println("Window 2:")
		fmt.Println(window2)
		fmt.Println("Max, max index:")
		fmt.Println(max, maxIndex)
		fmt.Printf("Window sizes: window 1: %d, window 2: %d\n", len(window1), len(window2))
	}

	// Plot the data on the console
	ks.Plot(window1, window2, max, maxIndex)

	if max > *deviation {
		fmt.Printf("CRITICAL: Deviation (%.2f) is greater than maximum allowed (%.2f)\n", max, *deviation)
		os.Exit(2)
	} else {
		fmt.Printf("OK: Deviation (%.2f) is within tolerances (%.2f)\n", max, *deviation)
		os.Exit(0)
	}
}
