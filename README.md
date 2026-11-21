# concierge

The storage and rules for a help desk: support tickets with their message threads, an append-only event log per ticket, attachment metadata, SLA-breach listing, and a public knowledge base of categories and articles with search.

It knows nothing about the platform it serves. Partitions are opaque, threads in an outside chat or email system are linked through free-form external references, and a read that crosses partitions (`ListAllTickets`, `ResolveTicketPartition`) must name who asked and why, and leaves an event on each ticket it reveals.

## Use

```sh
go get github.com/bbmumford/concierge
```

```go
store := concierge.NewSQLStore(db) // any database/sql driver; or concierge.NewMemStore()
if err := store.InitSchema(ctx); err != nil { /* ... */ } // idempotent; once at boot

err = store.CreateTicket(ctx, &concierge.Ticket{ /* partition, subject, requester ... */ })
err = store.AddMessage(ctx, &concierge.TicketMessage{ /* ticket, author, body ... */ })
late, err := store.ListBreachedTickets(ctx, 4*time.Hour, 72*time.Hour, time.Now())

articles, err := store.SearchArticles(ctx, partition, "reset password")
```

Everything goes through the `Store` interface, which both stores implement.

## Consumers

ORBTR library (its support host), the help.orbtr.io endpoint and io tools, and the node.orbtr.ai endpoint.
