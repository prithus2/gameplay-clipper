package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
)

func main() {
	apiKey := os.Getenv("GEMINI_API_KEY")
	if apiKey == "" {
		fmt.Println("Error: GEMINI_API_KEY environment variable is not set.")
		return
	}

	url := "https://generativelanguage.googleapis.com/v1beta/models?key=" + apiKey

	resp, err := http.Get(url)
	if err != nil {
		fmt.Printf("Failed to make request: %v\n", err)
		return
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		fmt.Printf("Failed to read response: %v\n", err)
		return
	}

	var result struct {
		Models []struct {
			Name            string   `json:"name"`
			SupportedMethod []string `json:"supportedGenerationMethods"`
		} `json:"models"`
	}

	if err := json.Unmarshal(body, &result); err != nil {
		fmt.Printf("Failed to parse JSON (Raw response below):\n%s\n", string(body))
		return
	}

	fmt.Println("Available Models supporting generateContent:")
	for _, m := range result.Models {
		for _, method := range m.SupportedMethod {
			if method == "generateContent" {
				fmt.Println("-", m.Name)
			}
		}
	}
}
