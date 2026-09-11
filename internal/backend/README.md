# Backend Implementation Notes

## Notation

### Predicates

| Notation | Meaning |
| :------- | :------ |
| $p \land q$ | $p$ and $q$ are true. |
| $p \lor q$ | $p$ or $q$ are true. |
| $\lnot p$ | $p$ is false. |

### Sets

| Notation | Meaning |
| :------- | :------ |
| $\empty$ | The empty set. |
| $\set{x}$ | The set with the single element $x$. |
| $\set{x \mid P(x)}$ | The set for all $x$ for which predicate $P(x)$ is true. |
| $x \in S$ | $x$ is an element of a set $S$. |
| $x \notin S$ | $x$ is not an element of a set $S$. |
| $\forall x \in S : P(x)$ | For all $x$ that are elements of set $S$, the predicate $P(x)$ is true. $\forall x \in \empty : P(x)$ is always true, regardless of $P(x)$. |
| $\exists x \in S : P(x)$ | There exists an $x$ that is an element set $S$, for which the predicate $P(x)$ is true. $\exists x \in \empty : P(x)$ is always false, regardless of $P(x)$. |

| Notation                        | Meaning              | Definition                                         |
| :------------------------------ | :------------------- | :------------------------------------------------- |
| $A \cap B$                      | Intersection         | $\set{x \| x \in A \land x \in B}$                 |
| $\bigcap_{A \in \mathcal{S}} A$ | Intersection of Sets | $\set{x \mid \forall A \in \mathcal{S} : x \in A}$ |
| $A \cup B$                      | Union                | $\set{x \| x \in A \lor x \in B}$                  |
| $\bigcup_{A \in \mathcal{S}} A$ | Union of Sets        | $\set{x \mid \exists A \in \mathcal{S} : x \in A}$ |
| $A \setminus B$                 | Difference           | $\set{x \mid x \in A \land x \notin B}$            |

### Derivations

- $I(x)$ is the set of derivations that have one or more outputs
  that derivation $x$ directly depends on.
  - $x \sim y$ is a relation equivalent to $y \in I(x)$.
  - $\forall x : x \notin I(x)$
- $I^+(x)$ is the transitive closure of derivations that derivation $x$ depends on.
  - $I^+(x) = \bigcup_{i \in I(x)} i \cup I^+(i)$.
  - $\forall x : x \notin I^+(x)$

## Build Algorithm

When the backend receives a build request
(the `RealizeRequest` from [zbstorerpc.go](../zbstorerpc/zbstorerpc.go)):

- Let $O$ be the set of derivations requested.
- Let $F$ be the set of fixed-output derivations.
- Let $U$ be the set of derivations $O \cup I^+(O)$.
- Let $G$ be the directed acyclic graph with vertices $U$
  and edges $\set{(x, y) | x \sim y}$.

Then:

1. Load $U$ into memory.
   These should all exist on disk because the edges of $G$ are store references.

1. **Gather existing realizations.**
   (This step produces a set of realizations
   and a map of derivations to derivation hashes.)
   Walk $G$ in dependency order
   (i.e. derivations with no input derivations first),
   selecting realizations and [hashing derivations][].
   Derivations can have realizations generated for them on the fly regardless of reuse policy,
   since their derivation hash is purely based on their output hash.
   New realizations may be downloaded from the fallback store
   if an output does not have any trusted realizations
   or an output does not have any local store paths.

1. **Obtain build roots.**

   Let $K$ be the set of derivations that have realizations.
   Let $D$ be the set of derivations whose realizations' outputs are present locally
   or can be downloaded successfully from the fallback store.
   Note that $F \subseteq K$ and $D \subseteq K$.

   $$
   v(x) = x \in F \lor (
    (x \notin O \lor x \in D) \land
    (\forall i \in I(x) : V(i)) \land
    (\forall i \in U : x \sim i \land w(i))
   )
   $$

   1. Check if we have the output store object(s) in the store.
      If so, mark as a build root and continue walking.
   2. Otherwise, attempt to download the output store object(s) from the fallback store.
      If the download succeeds, mark as a build root and continue walking.
   3. Otherwise, ignore all realizations we collected for this derivation
      and any realizations that transitively depend on this derivation.
      (We do this for the full derivation to avoid complexities with multi-output derivations.)
      We also ignore any derivation hash
      and clear the build root mark
      of any derivation that transitively depends on the derivation.
      Visit the failed derivation's transitive input derivations in breadth-first order,
      repeating "obtain missing build roots" steps 1-3 for each
      to download output store objects if possible.
      If we fail to download the outputs for a derivation that has no input derivations,
      then mark that derivation as a build root.

1. **Build what remains.**
   Walk $G$ in dependency order
   (i.e. derivations with no input derivations first),
   starting at each of the build roots marked in the previous step.
   For each derivation:

   1. Find any realizations whose output store objects exist in the store
      and are compatible with existing realizations.
      (This accounts for any concurrent builds.)
      If the derivation has multiple outputs that are needed for the build,
      then all of the derivation's outputs (not just the ones requested)
      must have acceptable realizations and be present in the store.
   2. If there is an acceptable realization, then use it.
   3. If there are any realizations whose output store objects do not exist in the store
      and are compatible with existing realizations,
      then attempt to download the output store objects from the fallback store.
   4. If the download(s) succeed, then use them.
   5. Download any realizations for the derivation from the fallback store.
   6. Repeat steps 1-4 with the new realizations.
   7. Otherwise, run the builder and record the realization(s) on success.

[hashing derivations]: https://main--zb-docs.netlify.app/binary-cache/realizations#derivation-hashes

## Store Concurrency

- The backend assumes at most one backend is running per-store-directory.
- The backend generally assumes that no other process will write to the store directory.
  (However, it is a bit more defensive about testing this assumption:
  if a store object is obviously missing or corrupted, it will complain.)
- The backend assumes that if and only if a store object is present in the store directory
  will a corresponding row exist in the `objects` table.
- The `inProgress` lock for a store path is acquired before accessing any store object.
  The lock is held while importing or accessing a store object.
  During access, the lock is released once it has been determined that the object exists.
  (Code may assume that if a store object exists at this time,
  it is fully constructed because of the previous bullet.)
  During import of a store object, the lock is released once it has been written to the filesystem
  and the row has been written to the `objects` table.
