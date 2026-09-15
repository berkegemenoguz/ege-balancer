package main

import (
	"fmt"
	"net/http"
	"time"
)

// probeTimeout bounds one health check, so that a balancer too stuck to answer
// is reported unhealthy instead of leaving the check hanging.
const probeTimeout = 2 * time.Second

// probe requests url and reports an error unless it answers 200. The image has
// no shell and no curl, so this is what a container health check runs.
func probe(url string) error {
	client := http.Client{Timeout: probeTimeout}

	response, err := client.Get(url)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("%s answered %s", url, response.Status)
	}
	return nil
}
