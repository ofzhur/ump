# Unus mundus

UMP is the Unus Mundus Provisioner. *Unus mundus* is Latin for "one
world": the idea that the many things of the world rest on one
underlying unity. This page says where the term comes from, what it
meant to those who used it, and what it stands for in ump.

## Meaning

C. G. Jung, who made the term known in the 20th century, put it this
way: "the idea of the unus mundus is founded on the assumption that the
multiplicity of the empirical world rests on an underlying unity, and
that not two or more fundamentally different worlds exist side by
side". In his use it is one reality beneath the empirical world, prior
to the split into psyche and matter, "the eternal Ground of all
empirical being": not the things merged into one, but the ground they
share.

## Origin

Jung took the phrase from Gerhard Dorn (born about 1530, died after
1584), a physician and alchemist, a follower of Paracelsus several of
whose works he translated into Latin. Jung sums up Dorn's *Physica
Trismegisti* thus: "In the beginning God created one world (unus
mundus). This he divided into two—heaven and earth." Dorn's own words
are that God "willed at first in his mind to create from nothing one
world" (*unum mundum*). The world was one in God's mind before it was
made; the division, Jung comments, "was necessary in order to bring the
'one' world out of the state of potentiality into reality".

The idea is older than the term. Jung calls this potential world "the
'mundus archetypus' of the Schoolmen", conjectures that Dorn's model
was Philo of Alexandria, and names Plotinus, who asked whether all
souls are one, as an earlier witness.

In his *Philosophia meditativa* Dorn describes three stages of the
union (*coniunctio*): first spirit and soul unite over against the
body, the *unio mentalis*; then that unity is joined with the body
again; last the whole is joined with "the first unity". Jung reads that
first unity as the unus mundus, "the potential world of the first day
of creation, when nothing was yet 'in actu,' i.e., divided into two and
many, but was still one". It is no merging with one's surroundings:
Dorn "expressly meant not a fusion of the individual with his
environment, or even his adaptation to it".

## Jung and Pauli

Jung discusses the term in *Mysterium Coniunctionis*, written from 1941
to 1954 and published in German in 1955 and 1956 (in English in 1963, as
volume 14 of his Collected Works). He reads it in modern terms: the
common background of microphysics and depth psychology "is as much
physical as psychic and therefore neither, but rather a third thing, a
neutral nature which can at most be grasped in hints since in essence
it is transcendental". He calls the concept "a metaphysical
speculation", and later "at least a probable hypothesis".

The physicist Wolfgang Pauli corresponded with Jung from 1932 to 1958,
and the two published *Naturerklärung und Psyche* together (1952; in
English *The Interpretation of Nature and the Psyche*, 1955): Jung's
essay on synchronicity (meaningful coincidences without a causal link)
beside Pauli's on Kepler. In the essay on Kepler Pauli wrote: "It would
be most satisfactory of all if physis and psyche could be seen as
complementary aspects of the same reality." Later scholarship calls
this position dual-aspect monism, or the Pauli-Jung conjecture: the
mental and the physical as two aspects of one reality that is neither.
Harald Atmanspacher says of it, and of the related views of Eddington,
Wheeler, Bohm and Hiley, that they are "based on speculations that
obviously exceed the scope of contemporary quantum theory". It is a
philosophical idea, not a result of physics.

## In ump

Every provider has its own API, its own rescue system, its own stock
images and its own way of doing networking ([ISPs](isps.md)). That is
the surface: many worlds side by side. ump's design puts one world
beneath them.

- **One system from one profile.** A profile says what a machine
  should become, and ump makes any machine that, a dedicated server
  as well as a cloud VM: the same operating system in the same shape,
  with the same disk layout, users and keys, interface names, firewall
  and tools ([operating systems](oses.md#what-every-system-gets),
  [profiles](profiles.md)).
- **One common ground.** A provider's way in, its rescue system or a
  rebuild with UMP's key, serves at most once, to get root over SSH;
  from there the server `kexec`s into UMP's own rescue system, a Debian
  that runs from RAM with no disk mounted, the same at every provider.
  Where the running system lets UMP in as root, ump enters it from
  there, without the provider's. "Rescue → partition → unpack →
  configure → reboot" is thus the same work everywhere ([the rescue
  system](concepts.md#the-rescue-system), [how a reinstall
  runs](concepts.md#how-a-reinstall-runs)).
- **Differences at the edges.** A provider answers a few questions:
  which servers exist, how to reset them, how to get into a rescue
  system. Its quirks stay in its package, and everything after the way
  in is one code path for bare metal and clouds ([design in
  brief](concepts.md#design-in-brief)).
- **One listing.** `ump server list` shows the servers of every
  account in one table, the same columns whatever the provider, with
  the servers no provider lists added by hand; the listing is the
  inventory of the fleet ([inventory](inventory.md)).
- **One home.** Everything ump keeps, the accounts, the profiles, its
  key, what it knows about the servers, is one state: a directory of
  your own, or a backend a team shares, so that the team acts as one
  UMP ([the state backend](state.md)).
- **One address space.** A private network gives every server ump
  installs an address of its own, whichever provider hosts it, kept
  across reinstalls and never another server's; the roles that connect
  the servers, a mesh per network, take the addresses from the
  inventory ([private networks](networks.md)).

## What the name does not claim

ump is a tool. The name is an image for its design, not a doctrine it
holds: it says nothing about psyche and matter, physics or
synchronicity, and Jung himself offered the concept as a speculation
and a hypothesis. Nor does ump make the machines one: they keep their
hardware, locations, prices and providers, and some of those
differences show, as [ISPs](isps.md) says provider by provider. And the
order is reversed: Dorn's one world comes first and the many follow
from it; ump's is made, by UMP's rescue system, its installer and a
profile, on machines that start out different.

## Sources

- C. G. Jung, *Mysterium Coniunctionis*, Collected Works, vol. 14,
  translated by R. F. C. Hull, 2nd ed., Princeton University Press,
  1970: pars. 659-664 and 759-775 (the latter the section "The Third
  Stage: The Unus Mundus"). Dorn's *Physica Trismegisti* and
  *Philosophia meditativa* (in *Theatrum chemicum*, vol. I) are quoted
  there in Jung's translation, Dorn's words on the one world in the
  note to par. 659.
- C. G. Jung and W. Pauli, *The Interpretation of Nature and the
  Psyche*, Pantheon, 1955, Pauli's sentence on p. 210 (German:
  *Naturerklärung und Psyche*, Rascher, 1952).
- C. A. Meier (ed.), *Atom and Archetype: The Pauli/Jung Letters
  1932-1958*, Princeton University Press, 2001.
- H. Atmanspacher, ["Quantum Approaches to
  Consciousness"](https://plato.stanford.edu/entries/qt-consciousness/),
  *Stanford Encyclopedia of Philosophy*, sections 5.1 and 5.2.
- H. Atmanspacher, "Dual-aspect monism à la Pauli and Jung", *Journal
  of Consciousness Studies* 19 (9-10), 2012, pp. 96-120.
- C. Gilly, ["Dorn, Gerhard"](https://hls-dhs-dss.ch/de/articles/042845/),
  *Historisches Lexikon der Schweiz*.
