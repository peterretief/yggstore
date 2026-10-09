package devices

type owner struct{}

func ownerOf(string) owner { return owner{} }

func (owner) restore(string) {}
