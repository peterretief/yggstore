package dashboard

// diskSpace is not reported on Windows.
func diskSpace(path string) (free, total int64) { return 0, 0 }
