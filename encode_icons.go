package main

import (
	"encoding/base64"
	"fmt"
	"os"
)

func main() {
	files := []string{
		"new_sync-removebg-preview.png",
		"new_sync_completed-removebg-preview.png",
		"new_login_req-removebg-preview.png",
	}

	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			fmt.Printf("Error reading %s: %v\n", f, err)
			continue
		}
		b64 := base64.StdEncoding.EncodeToString(data)
		fmt.Printf("// %s\nvar icon%s = \"%s\"\n\n", f, f, b64)
	}
}
