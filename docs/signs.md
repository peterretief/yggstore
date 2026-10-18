# Messages for signs: pub/sub for someone on a box

Status: proposed on 2026-10-18, waiting for approval. Some of it depends
on how Tim's signs take their messages now, which we still need to ask
him (see the questions at the end).

## Why

Tim makes electronic signs and wants to change what they show from one
place. The group's topics ([messaging.md](messaging.md)) fit this well:
- a node that was off catches up when it's back;
- there's no broker of anyone else's to depend on.

Topics are between nodes, though, and Tim is a person on a box. His
signs aren't nodes either: they're small devices on the internet, not on
Yggdrasil.

## What it gives him

- On his files page, a **Signs** card lists his signs. He types what each
  one should show and presses Send, to one sign or to a group of them,
  such as "all shop windows".
- Each sign gets its message from **any of the group's web nodes**, over
  ordinary https, at an address of its own:

  ```
  https://tim.withcare.co.za/_yggstore/sign/lobby?k=SIGNKEY
  ```

  It answers with the latest message for that sign. That's the last one
  sent, so a sign that was off just asks again when it's back. With
  `&wait=60` the request is held open until something new comes, so the
  sign sees a change in a second or two without asking every second.
  With `&stream=1` the messages come as server-sent events instead.
- The card shows when each sign last asked: "lobby, 2 minutes ago". A
  sign that stops asking is easy to spot.
- For his own programs, a send key: `POST` to the same address with
  `Authorization: Bearer SENDKEY` and the message as the body. A
  schedule, a spreadsheet or a till could then change the signs.

Messages are text or JSON, up to 64 KB, and he chooses the format. Images
and video come later, stored in the group and sent as a stub, the way
sites are.

## How it works

```
Tim's files page or program  ->  his box publishes the topic  sign/tim.withcare.co.za/lobby
                                  every web node follows sign/...
                                  and keeps the latest message per sign
sign  --https-->  any web node  ->  the latest message for lobby
```

1. **Sending.** The box publishes to the topic
   `sign/NAME/SIGN`, where NAME is the person's address. Web nodes take
   these topics only from the box that holds NAME, just as they take
   sites, so nobody else can change Tim's signs.
2. **Keeping.** Every web node follows `sign/` and keeps the latest
   message for each sign. While one web node is up, signs get their
   messages.
3. **The sign list.** The box publishes the list of Tim's signs, the
   groups and a hash of each sign's key, on the same topics. Web nodes
   check a sign's key against it. A key can be replaced from the card if
   a sign is lost.
4. **When the box is off,** signs still get the latest message from the
   web nodes, but Tim can't send a new one until the box is back. Step 2
   below closes that gap: a web node takes his send key itself and passes
   the message on for the box.

## What to know

- Like all topics, sign messages are readable on the group's nodes. For a
  sign in a shop window that doesn't matter. Anything private shouldn't
  go on a sign anyway.
- A sign needs https and a little code to ask for its message: an ESP32
  or a Raspberry Pi can do this. If Tim's signs speak MQTT instead, a web
  node can also offer the same messages over MQTT. That's step 3, built
  only if he needs it.
- It's meant for tens of signs per person, not thousands.

## Build order, once approved

1. Sign topics from the box, the web nodes keeping the latest, the sign's
   address with `wait`, and the **Signs** card. Try it with a test sign
   (a browser tab, then a small device).
2. Send keys at any web node, so sending works while the box is off.
3. Only if his signs need it: MQTT, images through stubs, or a schedule
   (show X from 8 to 5).

## Questions for Tim

1. How do the signs get their messages now? Do they read a file, check a
   web address, use MQTT, or something else?
2. What's in them? Is it a microcontroller such as an ESP32, a Raspberry
   Pi, or something made by another company?
3. How do they reach the internet: Wi-Fi, a SIM, or a wire?
4. What does a message look like: a line of text, several lines, colours,
   images?
5. About how many signs are there, and do they belong to him or to his
   customers? If his customers own them, each customer might need their
   own card and keys.
