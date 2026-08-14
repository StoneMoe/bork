# HTTP BitTorrent Tracker

The tracker serves BEP 3 HTTP announces at `/announce` and always returns the
compact peer format allowed by BEP 23. IPv6 peers use the BEP 7 `peers6` field,
and the observed source address uses the BEP 24 `external ip` field. Each
`info_hash` is kept in its own Durable Object, and registrations expire after
15 minutes. A Durable Object alarm removes expired peers even when no later
announce arrives. Each swarm stores at most 500 peers and each request reads at
most one bounded swarm record. A new registration replaces the least recently
seen peer when the swarm is full. Clients are asked to announce every 5 minutes.

The request must include the standard `info_hash`, `peer_id`, `port`,
`uploaded`, `downloaded`, and `left` fields. `started`, `completed`, and
`stopped` events are accepted. When an announce includes `ip`, that valid IPv4
or IPv6 address is registered instead of the request source address. The source
address is still returned as `external ip`.

`/announce` accepts only requests whose `User-Agent` starts with `Bork`.

Deploy from this directory with:

```console
wrangler deploy
```
