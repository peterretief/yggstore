# Pub/sub use cases: ideas to test with real people

Status: working notes, not a commitment. Each idea needs a conversation with
someone who has the problem before any code is written.

## What yggstore pub/sub gives you (from [messaging.md](messaging.md))

- Topics any node can publish to and follow; no broker or central server.
- Store-and-forward: a node that was off gets what it missed (sender retries
  for a week; new followers get 30 days).
- Messages up to 64 KB; anything bigger is stored and sent as a stub.
- Known sender: messages come over Yggdrasil from a known node.
- Sharded encrypted storage for the bulky part (video, audio, logs).

## What it does not give you (yet)

- **Topics are not secret from group members.** Any member can follow any
  topic, and bodies are readable on receiving nodes. For anything sensitive,
  either run a private group for that purpose or seal the body to the
  recipient's key before publishing (a stub sealed for one node already works
  this way).
- **No delivery receipts or escalation.** An alert that nobody acknowledges
  needs an application layer: ack topic, timeout, then the next contact.
- **Not a certified alarm path.** Position as a private or secondary channel.
- **Latency and uptime guarantees are untested.** Measure before promising.

## Common pattern

```
sensor/camera -> local agent -> publish "alerts/<site>/<kind>" (sealed)
                                 + stub of the evidence (sharded clip)
monitor/family node follows the topic -> acts -> publishes "ack/<alert id>"
```

## Use cases

| # | Use case | Publisher | Follower | Why pub/sub fits | Main risk |
|---|---|---|---|---|---|
| 1 | Alarm/intrusion to a security firm | Panel, camera agent | Monitoring firm | Works with firm node offline; no vendor cloud | Certification, trust in unreviewed crypto |
| 2 | Camera clips as sharded evidence | Camera agent | Firm, owner | Footage already off-site if recorder is stolen | Upload bandwidth, key sharing |
| 3 | **Fall alerts for older people** | Wearable, floor radar, bed/door sensor, phone | Family, carer, call centre | Private; no subscription cloud; family nodes catch up when back online | False alarms, no ack/escalation, device battery, medical-device rules |
| 4 | Daily "all fine" check-ins | Home hub | Family | Absence of a heartbeat is the alert | Needs a missed-heartbeat watcher |
| 5 | Medication reminders and taken/not-taken | Pill dispenser | Carer | Log is kept 30 days; carer sees history | Privacy of health data |
| 6 | Care home / sheltered housing rounds | Room sensors, staff devices | Duty staff, manager | Local-first, survives internet loss | Staff rota logic, regulation |
| 7 | Managed IT/security provider fleet status | Customer boxes | Provider | Heartbeats and health topics, no inbound ports | Scale (tens of nodes today) |
| 8 | Flood, smoke, freezer, door-open alerts | Cheap sensors via MQTT bridge | Owner, neighbours | Neighbour watch without a cloud | Needs an MQTT-to-topic bridge |
| 9 | Technician dispatch and job notes | Firm | Field staff | Private job notices, offline catch-up | Competes with mature tools |
| 10 | Neighbourhood / street watch | Households | Neighbours | Group is already a trust circle | Group moderation, false reports |
| 11 | Small business: till/door/cold-chain logs | Sensors | Owner, auditor | Tamper-evident history in sharded storage | Compliance wording |
| 12 | Remote sites: farms, boats, holiday homes | Sensors | Owner | Works over poor links with retry | Bandwidth, power |

## Fall alerts for older people: notes

- **Detection is the hard part, not messaging.** Wearables, ceiling/floor
  radar, pressure mats and camera analytics each have different false
  alarm rates and privacy trade-offs. Radar and pressure sensors are
  camera-free, which families often prefer.
- **Pub/sub needs an acknowledgement loop.** Publish the alert, wait for an
  `ack/<alert id>` from any carer, escalate to the next on a timer. The
  ack/escalation logic is the product.
- **Who runs a node?** Older people will not. A hub box (or a Raspberry Pi)
  installed by a family member or a local provider is the realistic model.
  A phone app that follows topics may be needed for carers.
- **Privacy is the selling point.** Health and location data stay between
  the family's nodes; no company holds it.
- **Regulation.** If it is marketed as a medical alarm, rules apply (in the
  UK, TSA code of practice; in the EU, medical-device regulation). Safer to
  start as a "family notification" product.
- **Incumbents to study:** pendant alarm and telecare services, Apple Watch
  fall detection, Philips Lifeline, Tunstall, Vayyar/radar sensors, Home
  Assistant plus MQTT setups.

## Who to ask, per use case

| Use case | People |
|---|---|
| 1, 2, 7, 9 | Alarm installers, monitoring firms, MSPs |
| 3, 4, 5 | Adult children of elderly parents, occupational therapists, telecare providers, local council or charity alarm schemes |
| 6 | Care home managers, sheltered housing wardens |
| 8, 12 | Home Assistant users, farmers, boat and holiday-home owners |
| 10, 11 | Community groups, small shop owners |

## Questions that work (ask about the past)

1. "Tell me about the last time an alert or message didn't reach the right
   person. What happened?"
2. "What do you use today, and what does it cost per month?"
3. "What happens when the internet or the vendor's service is down?"
4. "Who else has to be told, and how do you know they saw it?"
5. "Has anyone worried about who can see this data?"
6. "What would make you stop using it?"

Record: what they pay now, how often it fails, who decides to buy.

## What to study among current solutions

- Home/IoT: MQTT (Mosquitto, Home Assistant, Frigate), ntfy, Pushover.
- Security: Contact ID / SIA over IP, vendor clouds (Ajax, Hik-Connect).
- Telecare: pendant alarms, Lifeline-style services, wearable fall detection.
- Decentralised: Matrix, Nostr, Waku, libp2p GossipSub.

For each, note price, who holds the data, what happens offline, and the
complaints in reviews and forums.

## Suggested order of validation

1. Fall alerts and daily check-ins (3, 4): emotional, privacy-driven, simple
   hardware, family already trusted.
2. Security firm alerts plus evidence clips (1, 2): clearer paying buyers.
3. MSP fleet status (7): easiest to build, smaller emotional pull.

## Prototype that would prove the idea cheaply

A script that reads an MQTT sensor topic from Home Assistant, publishes a
sealed alert to `alerts/<home>/fall` through the local API (`/v1/publish`),
and a follower that publishes `ack/<id>` or escalates after N minutes. No new
yggstore code is needed for this.
