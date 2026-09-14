# Licensing: what you can do with Vornik

Plain answers to the questions companies and fork maintainers actually ask.
This page explains; the licence texts govern. Vornik Community Edition is
[AGPL-3.0](https://github.com/grinco/vornik/blob/main/LICENSE). The
companion plugins under `contrib/` are Apache-2.0. The full path-by-path map
is [LICENSING.md](https://github.com/grinco/vornik/blob/main/LICENSING.md)
at the repository root.

This is not legal advice. If a decision turns on it, have counsel read the
licence.

## Using Community Edition

**Can we run it inside our company, for commercial purposes, without
paying anyone?** Yes. The AGPL does not distinguish personal from
commercial use, and unmodified use carries no obligation at all. You do not
need to tell us, sign anything, or publish anything.

**We run it unmodified and our staff use its web UI and API over the
network.** Still nothing to do. The source-offer duty in AGPL section 13
applies only to a version *you have modified*.

**We modified it and our staff, or our customers, use it over the
network.** Then the people who interact with your modified version must be
offered its source, under the AGPL. In practice: put your fork on a Git host
they can reach, or add a link in the UI. That is the whole obligation. You
do not have to send anything to us.

**We modified it for an internal batch job. Nobody interacts with it over
a network and we do not distribute it.** No obligation. Private modification
is free.

**We ship it inside our own product or appliance.** That is distribution.
The recipient gets AGPL rights to the Vornik part and to your modifications
of it. Code that merely talks to Vornik over its API or MCP is not a
modification and is not affected. If you need to ship a closed derivative,
that is what the Enterprise licence is for.

**Does the AGPL "infect" the code our agents write, or the projects Vornik
works on?** No. Output produced by running the software is yours. The
licence covers the Vornik program, not what it produces.

## Forking

**Can we fork and keep our fork private?** Yes, as long as nobody outside
your organisation interacts with it over a network and you do not
distribute it.

**Can we fork publicly and never contribute back?** Yes. Contributing back
is welcome and never required.

**Do we need to sign the CLA to fork or use Vornik?** No. The Contributor
License Agreement applies only when you send a change to the upstream
repository. Using, forking and modifying need no agreement with anyone.

**What may our fork be called?** Anything except Vornik. "Fooer, a fork of
Vornik" is fine. The name and logo are trademarks and are not covered by the
code licence; the rules are in
[TRADEMARKS.md](https://github.com/grinco/vornik/blob/main/TRADEMARKS.md).

**Is there a licence check, a phone-home, or a kill switch we would have to
remove?** No. Edition is compiled in; Community builds simply do not contain
the Enterprise code. There is nothing to remove and nothing to circumvent.

## Contributing

**Why is there a CLA?** Vornik is open-core. The Community Edition is AGPL;
the same core also ships inside the proprietary Enterprise Edition. The CLA
grants EaseIT Labs the right to include your contribution in both. You keep
your copyright, and your contribution stays available to everyone under the
AGPL forever.

**Does the CLA assign my copyright?** No. It is a licence, not an
assignment. You remain the owner.

**I contribute as part of my job.** Your employer may own what you write.
The CLA asks you to confirm you have the right to contribute; if your
employer needs to sign, ask for the corporate CLA at `cla@vornik.io`.

## Enterprise Edition

**What is different about it?** It is a closed binary with the capabilities
in the [feature matrix](editions.md), sold under a commercial licence with
support. It is not AGPL and its source is not published. Buying it is also
how a company gets Vornik without AGPL obligations for a modified or
embedded deployment.

**If we stop paying, what happens?** Your Community Edition rights never
depended on the Enterprise licence and are unaffected. Configuration written
for Enterprise loads on Community; the Enterprise capabilities simply are not
there.

## Who owns Vornik

Copyright in the Vornik code is held by EaseIT Labs s.r.o. (Czech Republic)
together with the contributors who license their work under the CLA. The
Community Edition is published by EaseIT Labs under the AGPL-3.0, which is
irrevocable for every version already released.

Questions not answered here: `legal@vornik.io`. We would rather answer than
leave a grey area.
