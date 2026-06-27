package main

import (
	"fmt"
	"net/http/httptest"
	"os"
	"vornik.io/vornik/internal/ui"
)

func main() {
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	ui.NewServer().Handler().ServeHTTP(rr, req)
	fmt.Fprintln(os.Stderr, "status", rr.Code, "len", rr.Body.Len())
	_, _ = os.Stdout.Write(rr.Body.Bytes())
}
