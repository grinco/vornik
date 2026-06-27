package api

import (
	"context"
	"net/http"
)

func authDisabledReq(req *http.Request) *http.Request {
	return req.WithContext(context.WithValue(req.Context(), authEnabledKey, false))
}
