package concierge

import "time"

// The clone helpers below are what make MemStore a reference the durable store can
// be certified against: every read returns a deep copy, so a caller that mutates
// what it was handed changes nothing in the store, the way a row read out of
// SQLStore is already detached.
//
// WHAT THEY CONSUME: a stored *Ticket or *TicketMessage and the maps and time
// pointers hanging off it. WHAT CONSUMES THEM: MemStore's own reads and mutators.
//
// WHY A SEPARATE FILE: they are the only part of MemStore that touches no map on
// the receiver, and keeping them beside the methods pushed memstore.go past the
// length this package holds its files to.

func cloneTicket(t *Ticket) *Ticket {
	cp := *t
	cp.FormData = cloneAnyMap(t.FormData)
	cp.ExternalRefs = cloneStrMap(t.ExternalRefs)
	cp.ConfirmedAt = clonePtr(t.ConfirmedAt)
	cp.FirstResponseAt = clonePtr(t.FirstResponseAt)
	cp.ResolvedAt = clonePtr(t.ResolvedAt)
	cp.ClosedAt = clonePtr(t.ClosedAt)
	return &cp
}

func cloneMessage(m *TicketMessage) *TicketMessage {
	cp := *m
	cp.ExternalRefs = cloneStrMap(m.ExternalRefs)
	if m.Synced != nil {
		cp.Synced = make(map[string]bool, len(m.Synced))
		for k, v := range m.Synced {
			cp.Synced[k] = v
		}
	}
	return &cp
}

func clonePtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	c := *t
	return &c
}

func cloneStrMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	c := make(map[string]string, len(m))
	for k, v := range m {
		c[k] = v
	}
	return c
}

func cloneAnyMap(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	c := make(map[string]any, len(m))
	for k, v := range m {
		c[k] = v
	}
	return c
}
