# Changelog

## 0.1.0 (2026-11-25)


### Features

* a help desk over an injected Store: tickets, threaded messages, an event log, attachments and a knowledge base ([ce0ff14](https://github.com/bbmumford/concierge/commit/ce0ff14d1c87d2f25c37b9ba2e79b3db57340530))


### Bug Fixes

* a KB write naming an id another row holds is refused with ErrDuplicateID by both stores, and a refused write leaves the caller's struct as given ([cdb83a8](https://github.com/bbmumford/concierge/commit/cdb83a8bfa5f0e23a544763a593d71ed702969a4))
* a KB write the engine refuses for another reason returns the engine's error, whichever row holds the id it names ([dfb6751](https://github.com/bbmumford/concierge/commit/dfb6751a1ffd899b28d8206195d9bbd8182618ea))
* an article slug read without its category answers the published article whose category sorts first, on both stores and every call ([80ecc1d](https://github.com/bbmumford/concierge/commit/80ecc1dac998fd341219ee37275fd3307fdaa83f))
* CreateTicket over a held id answers ErrDuplicateID on both stores, and a short id is its own number ([d03d357](https://github.com/bbmumford/concierge/commit/d03d357975bec811254fbd7361b724cf394f83f3))
* dotted external ref keys read back from the SQL store ([a3dcc5b](https://github.com/bbmumford/concierge/commit/a3dcc5b5e1f8375cdc53716975b601be58835f0c))
* tickets, categories and articles refuse an empty partition ([b147b2f](https://github.com/bbmumford/concierge/commit/b147b2fd25d40f9a43a9835b5a0557ffb4bf5449))
