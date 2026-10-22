package ghcache

import "time"

// SetNow replaces the clock that dates answers. Call it before the client is used.
func (c *Client) SetNow(now func() time.Time) { c.now = now }
