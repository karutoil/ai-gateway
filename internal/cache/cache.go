package cache

import "net/http"

type Cache interface {
	Get(key string) (body []byte, status int, headers http.Header, ok bool)
	Set(key string, body []byte, status int, headers http.Header, ttlSeconds int)
	Invalidate(pattern string)
}
