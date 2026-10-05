package files

import "context"

type progressKey struct{}

// WithProgress returns a context under which Put, PutReader, PutFolder, Get,
// ReadBack and Restore call f with the plaintext bytes of each chunk once it
// has been stored or rebuilt. Chunks upload in parallel, so f must be safe to
// call from several goroutines.
func WithProgress(ctx context.Context, f func(bytes int)) context.Context {
	return context.WithValue(ctx, progressKey{}, f)
}

func progress(ctx context.Context, n int) {
	if f, ok := ctx.Value(progressKey{}).(func(int)); ok {
		f(n)
	}
}
