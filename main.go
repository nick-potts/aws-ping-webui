package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/ekalinin/awsping"
	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
)

type PingResult struct {
	Region     string  `json:"region"`
	Code       string  `json:"code"`
	Latency    float64 `json:"latency"`
	ClientPing float64 `json:"clientPing"`
	Error      string  `json:"error,omitempty"`
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

func pingClient(ipStr string) float64 {
	// Parse IP address
	ip := net.ParseIP(ipStr)
	if ip == nil {
		log.Printf("Invalid IP address: %s", ipStr)
		return 0
	}

	// Create ICMP connection using unprivileged UDP
	c, err := icmp.ListenPacket("udp4", "0.0.0.0")
	if err != nil {
		log.Printf("Error creating ICMP connection: %v", err)
		return 0
	}
	defer c.Close()

	// Create ICMP message
	msg := icmp.Message{
		Type: ipv4.ICMPTypeEcho,
		Code: 0,
		Body: &icmp.Echo{
			ID:   os.Getpid() & 0xffff,
			Seq:  1,
			Data: []byte("PING"),
		},
	}

	// Serialize message
	msgBytes, err := msg.Marshal(nil)
	if err != nil {
		log.Printf("Error marshaling ICMP message: %v", err)
		return 0
	}

	// Send ping and measure time
	start := time.Now()
	_, err = c.WriteTo(msgBytes, &net.UDPAddr{IP: ip})
	if err != nil {
		log.Printf("Error sending ICMP packet: %v", err)
		return 0
	}

	// Wait for reply
	reply := make([]byte, 1500)
	err = c.SetReadDeadline(time.Now().Add(time.Second * 2))
	if err != nil {
		log.Printf("Error setting read deadline: %v", err)
		return 0
	}

	n, _, err := c.ReadFrom(reply)
	if err != nil {
		log.Printf("Error reading ICMP reply: %v", err)
		return 0
	}

	duration := time.Since(start)

	// Parse reply
	_, err = icmp.ParseMessage(1, reply[:n]) // Use 1 for ICMP protocol number
	if err != nil {
		log.Printf("Error parsing ICMP reply: %v", err)
		return 0
	}

	return float64(duration.Milliseconds())
}

func streamHandler(w http.ResponseWriter, r *http.Request) {
	log.Println("Starting new ping request...")

	// Get client IP
	ip := r.Header.Get("X-Forwarded-For")
	if ip == "" {
		ip = r.RemoteAddr
		if colonIndex := strings.LastIndex(ip, ":"); colonIndex != -1 {
			ip = ip[:colonIndex]
		}
	}
	clientPing := pingClient(ip)
	log.Printf("Client ping to %s: %.2fms", ip, clientPing)

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
				Region:     region.Name,
				Code:       region.Code,
				Latency:    float64(minLatency.Milliseconds()),
				ClientPing: clientPing,
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

func indexHandler(w http.ResponseWriter, r *http.Request) {
	regions := awsping.GetRegions()

	// Start of HTML
	fmt.Fprint(w, `<!DOCTYPE html>
<html>
<head>
    <title>AWS Region Pinger</title>
    <style>
        body {
            font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif;
            max-width: 1200px;
            margin: 0 auto;
            padding: 20px;
            background: #f5f5f5;
        }
        .client-ping {
            background: white;
            padding: 15px;
            margin-bottom: 20px;
            border-radius: 4px;
            box-shadow: 0 1px 3px rgba(0,0,0,0.1);
        }
        .client-ping .value {
            font-family: monospace;
            font-weight: bold;
        }
        table {
            width: 100%;
            border-collapse: collapse;
            background: white;
            box-shadow: 0 1px 3px rgba(0,0,0,0.1);
            border-radius: 4px;
        }
        th, td {
            padding: 12px;
            text-align: left;
            border-bottom: 1px solid #eee;
        }
        th {
            background: #f8f9fa;
            font-weight: 600;
        }
        .pending {
            color: #666;
            font-style: italic;
        }
        .error {
            color: #dc3545;
        }
        .latency {
            font-family: monospace;
            font-size: 14px;
        }
    </style>
</head>
<body>
    <h1>AWS Region Pinger</h1>
    <div class="client-ping">
        Your ping: <span class="value" id="clientPing">Measuring...</span>
    </div>
    <table id="results">
        <thead>
            <tr>
                <th>Region</th>
                <th>Code</th>
                <th>Latency</th>
                <th>Status</th>
            </tr>
        </thead>
        <tbody>`)

	// Add each region
	for _, region := range regions {
		fmt.Fprintf(w, `
            <tr data-code="%s">
                <td>%s</td>
                <td>%s</td>
                <td class="latency">Pending...</td>
                <td class="status pending">Pinging...</td>
            </tr>`, region.Code, region.Name, region.Code)
	}

	// End of HTML
	fmt.Fprint(w, `
        </tbody>
    </table>

    <script>
        const tbody = document.querySelector('#results tbody');
        const clientPingElement = document.getElementById('clientPing');
        const evtSource = new EventSource('/ping');
        
        evtSource.onmessage = (event) => {
            const result = JSON.parse(event.data);
            
            // Update client ping if available
            if (result.clientPing !== undefined) {
                clientPingElement.textContent = result.clientPing.toFixed(2) + ' ms';
            }
            
            // Find the row
            const row = document.querySelector('tr[data-code="' + result.code + '"]');
            if (!row) return;
            
            // Update latency and status
            const latencyCell = row.querySelector('.latency');
            const statusCell = row.querySelector('.status');
            
            if (result.error) {
                latencyCell.textContent = 'N/A';
                statusCell.textContent = result.error;
                statusCell.className = 'status error';
            } else {
                latencyCell.textContent = result.latency.toFixed(2) + ' ms';
                statusCell.textContent = 'Success';
                statusCell.className = 'status';
            }
        };
        
        evtSource.onerror = () => {
            console.error('EventSource failed');
        };
    </script>
</body>
</html>`)
}

func main() {
	http.HandleFunc("/", indexHandler)
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
