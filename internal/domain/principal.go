package domain

// Principal is the authenticated caller of an API request. Kind is KindClient
// (a human machine, the owner in v0.x) or KindWorker.
type Principal struct {
	ID   ID
	Kind Kind
}

// ActorKind is how audit records the principal.
func (p Principal) ActorKind() ActorKind {
	if p.Kind == KindWorker {
		return ActorWorker
	}
	return ActorHuman
}
