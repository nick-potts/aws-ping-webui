package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/ekalinin/awsping"
)

type PingResult struct {
	Region  string  `json:"region"`
	Code    string  `json:"code"`
	Latency float64 `json:"latency"`
	Error   string  `json:"error,omitempty"`
}

func pingRegion(region awsping.AWSRegion) (time.Duration, error) {
	client := &http.Client{
		Timeout: time.Second * 10,
	}

	url := fmt.Sprintf("https://s3.%s.amazonaws.com/?ping=%d", region.Code, time.Now().UnixNano())
	req, err := http.NewRequest("HEAD", url, nil)
	if err != nil {
		return 0, err
	}

	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	return time.Since(start), nil
}

func streamHandler(w http.ResponseWriter, r *http.Request) {
	log.Println("Starting new ping request...")

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming unsupported!", http.StatusInternalServerError)
		return
	}

	regions := awsping.GetRegions()
	log.Printf("Got %d regions to ping", len(regions))

	results := make(chan PingResult, len(regions))
	var wg sync.WaitGroup
	wg.Add(len(regions))

	for i := range regions {
		go func(region awsping.AWSRegion) {
			defer wg.Done()

			log.Printf("Starting ping for region: %s", region.Code)

			var minLatency time.Duration
			var lastError error

			for i := 0; i < 3; i++ {
				latency, err := pingRegion(region)
				if err != nil {
					lastError = err
					continue
				}
				if minLatency == 0 || latency < minLatency {
					minLatency = latency
				}
				time.Sleep(time.Millisecond * 100)
			}

			result := PingResult{
				Region:  region.Name,
				Code:    region.Code,
				Latency: float64(minLatency.Milliseconds()),
			}

			if minLatency == 0 && lastError != nil {
				result.Error = lastError.Error()
				log.Printf("Error pinging %s: %v", region.Code, lastError)
			} else {
				log.Printf("Successfully pinged %s: %.2fms", region.Code, result.Latency)
			}

			results <- result
		}(regions[i])
	}

	go func() {
		wg.Wait()
		log.Println("All pings completed, closing results channel")
		close(results)
	}()

	for result := range results {
		data, err := json.Marshal(result)
		if err != nil {
			log.Printf("Error marshaling result: %v", err)
			continue
		}
		fmt.Fprintf(w, "data: %s\n\n", data)
		flusher.Flush()
		log.Printf("Sent result for region %s", result.Code)
	}

	log.Println("Finished streaming all results")
}

func main() {
	fs := http.FileServer(http.Dir("static"))
	http.Handle("/", fs)
	http.HandleFunc("/ping", streamHandler)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	log.Printf("Server starting on port %s...", port)
	if err := http.ListenAndServe(":"+port, nil); err != nil {
		log.Fatal(err)
	}
}
