package vault

import "context"

// Discover runs one discovery round, so tests don't have to drive Run's ticker.
func (r *Registry) Discover(ctx context.Context) (Changes, error) { return r.discover(ctx) }
