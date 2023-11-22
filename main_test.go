package main

import (
	"fmt"
	"testing"
)

func TestPostJSON(t *testing.T) {

	err := preview("https://x.com/DailyLoud/status/1727101147817689311?s=20", "twit.mp4")

	fmt.Println("err", err)
}
