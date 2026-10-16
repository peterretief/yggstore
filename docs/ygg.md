# Ygg: balancing what members give, and rewarding the people who make things

Status: proposal, 2026-10-16. Nothing here is built yet. It replaces the
separate credits and ygg drafts. The open standards it relies on were
checked on that date (sources at the end); they are moving, so check again
before building the money parts.

Two things, in one unit:
- **Balancing the work.** Some members give more than they use: a box with
  a big drive, on all day, holding other people's files and serving the
  group's sites. Others use more than they give. The group should see who
  is ahead and behind, and settle up if it wants.
- **Rewarding makers.** People should be able to reward the many people
  whose work they enjoy online: a news piece, a song, a drawing, a poem.
  Each reward is tiny. Pressing "ygg" a hundred times a week across many
  sites should cost about as much as a coffee a month.

Both are counted in **yggs**: **1 ygg = $0.001**, a tenth of a US cent.

## What a ygg is, and isn't

A ygg is a unit of measure, fixed in US dollars. It isn't a coin or a
token. It can't be bought, sold, traded or handed on, and there is no
exchange. Why:
- **The group already trusts its admins** and knows its members, so it
  needs no blockchain. Each record is signed, and every member can check
  their own.
- **A price that moves would undo the point.** If yggs could be traded, the
  cost of storage, and the value of a reward, would rise and fall with a
  market.
- **Fixed in dollars, not rand,** because the rand moves a lot, and a ygg
  should be worth the same next year as today.
- **The group never holds anyone's money.** Money stays in each person's own
  wallet and only moves from one person's wallet to another's (see
  "Settling"). Taking people's money in and paying it out again is
  regulated: e-money in South Africa is for banks, crypto assets have been
  financial products since 2022, and an exchange needs an FSCA licence and
  FIC Act customer checks. Members settling what they owe each other,
  directly, is a much smaller thing, closer to a stokvel. (This is not
  legal advice; check before real money changes hands.)

## The local ledger

Each member has a **local balance** in yggs. It starts at 0, and it's a
record of who owes whom inside the group, not money held anywhere.
- **Work earns yggs, and the person it's done for spends them.** Anna's box
  holds Ben's files: Anna goes up, Ben goes down by the same amount.
- **Rewards move yggs.** Ben presses ygg on Anna's song: Ben goes down by 1
  and Anna goes up by 1.
- **So the balances always add up to 0:** someone's plus is someone else's
  minus. Nobody has to put money in to start.
- **Storing on your own boxes costs nothing.** It's your own space.
- **The group sets a limit** on how far below 0 anyone may go, counting
  rewards and storage together. Past it, the box admin has a word; holding
  back new uploads or rewards is for the group to decide.

Balances only ever settle to the person's own wallet. A balance can't be
passed to a third party or spent outside the group. That's what keeps it a
record between members and stops it acting like money.

### Netting

Before anything is paid, the amounts cancel out. If Anna gives Ben 300
yggs of rewards, Ben gives Anna 200, and Anna's box earns 150 holding Ben's
files, only one thing is left: Ben owes Anna 50. In a group where people
serve and reward each other, most of it cancels. So far fewer payments are
needed, more of them clear the wallets' minimum amounts, and much less goes
in fees.

## What earns yggs

Storage comes first, because it can already be measured and checked (see
below). The other kinds of work come later, each once it can be checked
too:

| Work | Measured as | Phase |
| --- | --- | --- |
| Holding others' shards | GB-months: bytes held × time, as the gateway already meters for customers | 1 |
| Holding paying customers' shards (gateway) | GB-months, paid for by the customers | 1 |
| Serving the group's sites (web node) | requests or bytes served, per site owner | later |
| Sending mail for members (mail relay) | messages sent, per sender | later |
| Being up | hours up, as a small bonus on top of storage | later |

The rate is the group's choice. A suggestion to start with: **10 yggs per
GB-month** ($0.01). That's about half what the big cloud providers
charge for ordinary storage, which seems fair for space among friends.

Shards take about 1.5× the file's size, because each piece of data is
stored as 6 shards, any 4 of which rebuild it. Yggs count the shards, so
storing a 10 GB file for a month costs about 15 GB-months, or 150 yggs
($0.15). This matches the gateway's monthly report.

**Paying customers** of the gateway count as one member, "Paying
customers", as the dashboard's Give and use table shows already. Their
side is settled by the gateway's operator out of what the customers pay,
as [gateway.md](gateway.md) describes, so the members holding their data
are paid like any other.

## Where the storage numbers come from, and why they can be trusted

Every node already reports how much it holds for each uploader
(`ByWriter` in `/v1/info`, from `localstore.UsageByWriter`). The Give and
use table on the dashboard adds these up. But a node could lie about that,
to earn yggs it hasn't earned. So a holding counts only when **both sides
agree**:

1. **The holder says** what it holds per writer, as it does today.
2. **The writer's own records say** which shards it put on that node (its
   stubs).
3. **The writer checks** that the shards are really there, with the
   storage challenges each stub already carries (`internal/challenge`: a
   node can only answer from the shard itself). These run on Verify today;
   the ledger needs them to run on a schedule too.

A day's earnings are the smaller of the holder's figure and the writer's
figure. A node that fails a challenge earns nothing for that shard until it
passes again. This keeps holders from claiming space they don't use, and
writers from denying space they do.

## Rewards

### Makers say where to be paid

Anyone with a site that yggstore serves sets a **wallet address** for it in
the dashboard, beside the site:
- members' sites: peter.withcare.co.za, and the sites they publish;
- people on a box: on their "Your website and messages" page;
- later, if their owners want, localbox's domains.

The web node then adds the Web Monetization tag,
`<link rel="monetization" href="…">`, to every page of that site, the same
way it adds the contact link today (`site.WithContactLink`). Visitors who
already have the Web Monetization browser extension start paying at once,
from their own wallets, with no yggstore involvement at all. This is the
simplest part, and useful on its own.

### The ygg button

A small **ygg** button on each page, for rewarding one thing in particular.
It needs no login, because it talks to **the visitor's own dashboard**, on
their own computer:
- The button opens `http://localhost:7480/ygg?to=<site>&page=<url>`.
- The dashboard shows "Give 1 ygg to anna.withcare.co.za for 'The river
  song'?", with Give, Give 5, and Cancel.
- Pressing Give records it. No money moves yet.

A page can't give yggs on its own: the dashboard asks, and only the person
at the computer can answer. The dashboard's existing guard already refuses
actions that other web pages try to send it. The confirm page also refuses
to be shown inside another page, so it can't be hidden under a decoy
button. People on a box use their box's dashboard instead of localhost.
Visitors who aren't members have no dashboard; the button sends them to
the extension, or to the maker's wallet page.

### Inside the group, and outside it

- **A maker in the group:** the reward goes on the local ledger, so it nets
  out against everything else between those two people.
- **A maker outside the group:** nothing to net against. The dashboard
  keeps a tally and pays them directly from the giver's wallet (see
  "Settling"). Makers without a wallet address yet have their yggs kept as
  promises, paid once they add one. Promises older than a year lapse.

### Privacy

What someone reads and listens to is private.
- **The page-by-page list** of what you rewarded stays on your own computer.
  The dashboard sends it nowhere.
- **The ledger records only each day's total** from one member to another,
  because netting and checking need it. Only those two members and the
  admin can see a row like that.
- **Makers** see the total that came in, per page or per site, but not who
  gave it, unless the giver chooses to sign their ygg.

Storage rows are different. Who holds how much for whom is open to the
whole group, as Give and use is today. Open books suit people who know
each other, and keep the admin honest too.

## The ledger, day by day

- **Each node's statement.** Once a day each node signs a statement: for
  each writer, the byte-hours it held for them, and the challenges it
  passed. It sends this to the admin, and to each writer it names, over the
  existing messaging.
- **Each writer checks its rows** against its own stubs and challenge
  results, and signs to agree or to dispute.
- **Each member's rewards.** Each member's dashboard signs the day's reward
  totals per maker in the group, and sends them to the admin, sealed so no
  one else can read them.
- **The admin adds up the agreed rows** into the day's ledger, signs it,
  and sends each member their own part, plus everyone's balances. Each
  member keeps a copy, so the history doesn't rest on one box.
- **Disputed rows stay out of the ledger** until they're resolved. The
  dashboard shows them to both people and to the admin.

Everything is kept as plain JSON files, one per day, like the gateway's
`usage/YYYY-MM.json`. A balance is the sum of that member's rows in every
ledger. Anyone can add their own up again and get the same answer.

## The group's own ygg wallet

The group runs a wallet of its own, but **only for yggs**. It never holds
dollars or rands.

- **Each member gets a wallet address** on the group's domain, such as
  `$pay.withcare.co.za/anna`, served from the admin box. Behind it is the
  member's local balance: the wallet is the ledger above, reached through
  an open standard.
- **It speaks Open Payments**, the same standard as the wallets used for
  settling. So the Web Monetization tag on members' sites, the ygg button
  and the ledger all work through one standard way of paying, and tools
  built for Open Payments work with it too.
- **Rewards between members go through it** and never touch a payment
  company. They cost nothing and can be as small as 1 ygg.
- **Nothing in it can be cashed out.** Balances still add up to 0, and
  still only settle from one member's own wallet to another's (see
  "Settling"). That keeps it a record between members, not money held for
  them.
- **It can't pay makers outside the group.** Paying them would mean real
  money passing through the group's wallet, which is the regulated part.
  Those rewards still go from the giver's own wallet.

**How to build it:** either run **Rafiki**, the Interledger Foundation's
open-source wallet software, with yggs as its only currency, or add a small
Open Payments server to yggstore itself. Rafiki is complete but heavy (it
needs databases and several services, and is meant for licensed
providers), so it would run on the admin box, not on a Pi. A server in
yggstore would be light and fit what the group already has, but means
implementing the standard's permissions (GNAP) and request signing. To be
decided once the ledger works.

## Settling

The local balance can stay just a score. Settling turns it into real money,
from one member's own wallet straight to another's.

**The standards this uses:**
- **Open Payments** is an open API that wallets implement, such as banks,
  digital wallets and mobile money providers. With it, a person's wallet
  can give an app permission to send money, up to a limit per period (say
  $5 a month), without the app ever holding the money.
- **Web Monetization** is a W3C Community Group draft built on Open
  Payments, used above for the tag on makers' pages.
- **Interledger** connects the wallets. Wallets that hold real money with
  it are licensed financial institutions. That's why the group's own wallet
  (above) holds yggs only, and real money settles between wallets run by
  licensed providers.

**How it goes:**
1. **Once, each member links their wallet** to their dashboard. Their wallet
   asks them to allow it, with a monthly limit they choose. The wallet
   enforces that limit, not yggstore.
2. **Once a month,** the admin's dashboard nets everyone's balances, then
   works out the fewest payments that clear them: who pays whom, and how
   much.
3. **Each member owing more than a threshold** (say 5,000 yggs, $5) has
   their dashboard pay their part to the members they owe, through their
   own wallet. Each payment is recorded in the ledger, and the balances
   move back towards 0.
4. **Amounts below the threshold, or below a wallet's minimum,** wait for
   next month. Wallets each have a smallest amount they can send, which
   varies by wallet and currency (one Web Monetization example found
   MX$0.18).
5. **Makers outside the group** are paid in the same run, from each giver's
   wallet, once their total clears the minimum.
6. **If a giver's wallet can't cover it,** the payment waits, and the limit
   below 0 caps how much anyone can owe meanwhile.
7. **A member can ask to settle early,** for example when leaving the group.

Members who are paid may owe tax on it. Each member gets a monthly
statement of what they paid and were paid.

**Wallets, as of October 2026:**
- Few wallets support Open Payments yet, and the ones that do mostly only
  pay other accounts at the same wallet. That's fine for settling, as long
  as every member uses the same one.
- **The official list has two:** the free **test wallet** with play money
  (wallet.interledger-test.dev), which everything is built and tried
  against first, and **GateHub** (USD, EUR, for the USA and EU). **Whether
  people in South Africa can open a GateHub account isn't confirmed:** try
  signing up.
- **Fynbos** (fynbos.app) is worth trying too. It is based in Cape Town and
  co-founded by one of the designers of Interledger. In 2024 it ran in the
  USA, Europe and South Africa, but only paid wallets in the same region.
  It isn't on the official list now, so check that it still takes sign-ups.
- **Interledger Wallet** (interledger.app), the Interledger Foundation's
  own: whether it's open in South Africa isn't known.
- **Rand wallets come later.** In June 2026 the Interledger Foundation was
  still planning a rand-to-euro route, not running one.
- Topping up a USD wallet from a rand account means converting money, and
  the bank or card takes a margin, often a few percent, so monthly top-ups
  are better than frequent ones. It also counts as sending money abroad,
  though a few dollars a month is far inside the yearly allowance.

### Or: settling in a USD stablecoin

A second way to settle, for members who prefer it, or if no Open Payments
wallet works from South Africa:
- **Members hold a USD stablecoin, such as USDC,** in a wallet app on their
  own phone. Each holds their own keys, so the group still never holds
  anyone's money.
- **Settling works the same way:** the admin's dashboard works out who pays
  whom, and each member who owes sends their part from their own wallet to
  the addresses of the members they owe. On a low-fee network a payment
  costs a fraction of a cent, so there's no minimum to wait for.
- **Members buy and sell it themselves,** in rands, through an exchange
  licensed in South Africa, such as VALR or Luno. The group doesn't buy,
  sell or exchange anything.
- **Yggs stay what they are:** a unit fixed in dollars, not a coin. The
  stablecoin is only how a balance is paid.

**What it costs:**
- **Keys:** a member who loses their phone and their recovery words loses
  the money in it. Nobody can reset it.
- **The coin is only as good as its issuer,** who holds the dollars behind
  it.
- **Mistakes are final:** a payment to a wrong address can't be called
  back, so the dashboard should only pay addresses the member set
  themselves, after showing them.
- **Rules:** tax on what members are paid, and whether South Africa's
  exchange-control limits count stablecoins, need checking with someone
  who knows the current rules before real money moves.

Open Payments stays the first choice, because members' money then stays
in an ordinary wallet with a monthly limit their wallet enforces. The
dashboard could offer both, with each member choosing, as long as each
pair settling with each other uses the same one.

## On the dashboard

A **Ygg** card under Give and use:
- your balance, with what earned it and what spent it this month (storage,
  rewards given, rewards received);
- the group's balances;
- any rows in dispute, with Agree and Dispute buttons;
- the limit, and how close you are to it;
- once settling is on: your linked wallet, its monthly limit, and the next
  settlement.

And the **Rewards** page: your own history of what you rewarded, kept on
your computer, and, for makers, what came in per page.

## Build order, once approved

No money moves until step 8, so the group can watch balances for a few
months and see whether they feel fair first.

1. **Signing keys.** Each member gets a signing key next to their sharing
   key. The sharing key seals things and can't sign, so it can't sign
   statements.
2. **Storage statements:** each node's daily statement, the writer's check
   against its stubs and challenges, and scheduled challenges.
3. **The admin's daily ledger,** sent to members, and the balances worked
   out from it.
4. **The Ygg card** on the dashboard, and the limit (a warning first).
5. **Wallet addresses** for sites, and the Web Monetization tag on their
   pages. Try it with the test wallet and the extension.
6. **The ygg button,** the dashboard's confirm page and the Rewards page,
   with rewards to members going on the ledger.
7. **Linking a wallet** with an Open Payments grant, and monthly netting and
   settling, all against the test wallet.
8. **Real money in USD,** starting with members who can use GateHub, then
   more wallets as they connect.
9. **Later:** the group's own ygg wallet, with wallet addresses for every
   member; settling in a stablecoin as a second option; yggs for web and
   mail work and the uptime bonus; and rand wallets.

## Decided later

- **The storage rate:** 10 yggs per GB-month, or something else?
- **The limit below 0,** and what happens past it: just a warning, or no
  new uploads or rewards until it's settled?
- **The settling threshold and how often:** $5 a month?
- **Whether balances ever expire,** or slowly fade, as some time banks do,
  so old balances don't pile up.
- **Who resolves disputes:** the admin alone, or the admin and one other
  member.
- **Signed yggs:** whether givers may attach their name to rewards.
- **Promises to outside makers:** how long they wait for a wallet; a year?
- **Localbox's domains:** whether to offer the wallet tag to their owners.
- **GateHub from South Africa:** whether members here can open an account.
  If not, try Fynbos and Interledger Wallet, or settle in a stablecoin.
- **The group's own wallet:** run Rafiki, or build a small Open Payments
  server into yggstore.
- **Stablecoin settling:** whether to offer it at all, and if so which coin
  and which network.

## Sources

- [Web Monetization: receiving payments](https://webmonetization.org/docs/intro/receiving-payments)
  and [the link element](https://webmonetization.org/docs/references/html-link-rel-monetization)
- [Web Monetization-compatible wallets](https://webmonetization.org/wallets/)
- [How Web Monetization uses Open Payments, part 2: payment sessions](https://interledger.org/developers/blog/web-monetization-open-payments-part-2-payment-sessions/)
  (minimum sendable amounts)
- [Web Monetization updates, May–June 2026](https://community.interledger.org/interledger/web-monetization-work-in-may-june-2026-p29)
- [Open Payments](https://github.com/interledger/open-payments) and
  [Rafiki](https://rafiki.dev/introduction/overview) (for regulated
  account servicing entities)
- [Open Payments-enabled wallets](https://openpayments.dev/resources/op-wallets/)
- [Interledger Foundation: wallet work week, June 2026](https://interledger.org/blog/wallet-work-week/)
  (the rand-to-euro route, being planned)
- [TechCentral: open payments in South Africa](https://techcentral.co.za/open-payments-interledger-summit/250962/)
  (Fynbos in the USA, Europe and South Africa, same region only)
- [Interledger community call, 2024](https://community.interledger.org/chiara/community-call-explained-3oh5)
  (Fynbos in Europe with GateHub)
