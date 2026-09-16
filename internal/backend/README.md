# Backend Implementation Notes

## Notation

For precision, these notes are written in terms of set theory.
This section introduces the notation used.

### Logic

| Notation | Meaning |
| :------- | :------ |
| $`p \land q`$ | $p$ and $q$ are true. |
| $`p \lor q`$ | $p$ or $q$ are true. |
| $`\lnot p`$ | $p$ is false. |

### Sets

| Notation | Meaning |
| :------- | :------ |
| $`\emptyset`$ | The empty set. |
| $`\mathbb{F}_2`$ | The finite field/set of boolean values. |
| $`\set{x}`$ | The set with the single element $x$. |
| $`\set{x \mid P(x)}`$ | The set for all $x$ for which predicate $P(x)$ is true. |
| $`x \in S`$ | $x$ is an element of a set $S$. |
| $`x \notin S`$ | $x$ is not an element of a set $S$. |
| $`\forall x \in S : P(x)`$ | For all $x$ that are elements of set $S$, the predicate $P(x)$ is true. $`\forall x \in \emptyset : P(x)`$ is always true, regardless of $P(x)$. |
| $`\exists x \in S : P(x)`$ | There exists an $x$ that is an element set $S$, for which the predicate $P(x)$ is true. $`\exists x \in \emptyset : P(x)`$ is always false, regardless of $P(x)$. |
| $`A \subseteq B`$ | $A$ is a subset of $B$, i.e. $\forall x \in A : x \in B$. |
| $`\mathcal{P}(A)`$ | The power set of $A$: the set of all subsets of $A$, including $`\emptyset`$ and $A$ itself. |
| $`f : A \to B`$ | $f$ is a function from domain $A$ to codomain $B$. |

### Set Relations

| Notation                          | Meaning              | Definition                                           |
| :-------------------------------- | :------------------- | :--------------------------------------------------- |
| $`A \cap B`$                      | Intersection         | $`\set{x \| x \in A \land x \in B}`$                 |
| $`\bigcap_{A \in \mathcal{S}} A`$ | Intersection of Sets | $`\set{x \mid \forall A \in \mathcal{S} : x \in A}`$ |
| $`A \cup B`$                      | Union                | $`\set{x \| x \in A \lor x \in B}`$                  |
| $`\bigcup_{A \in \mathcal{S}} A`$ | Union of Sets        | $`\set{x \mid \exists A \in \mathcal{S} : x \in A}`$ |
| $`A \setminus B`$                 | Difference           | $`\set{x \mid x \in A \land x \notin B}`$            |
| $`A \times B`$                    | Cartesian Product    | $`\set{(a, b) \mid a \in A \land b \in B}`$          |
| $`A \subseteq B`$                 | Subset               | $`\forall x \in A : x \in B`$                        |

### Derivations

The following are notations specific to zb:

- $`I(x)`$ is the set of derivations that have one or more outputs
  that derivation $x$ directly depends on.
  - $`x \sim y = y \in I(x)`$.
  - $`\forall x : x \notin I(x)`$
- $`I^+(x)`$ is the transitive closure of derivations that derivation $x$ depends on.
  - $`I^+(x) = \bigcup_{i \in I(x)} i \cup I^+(i)`$.
  - $`\forall x : x \notin I^+(x)`$
- $F$ is the set of fixed-output derivations.

## Build Algorithm

When the backend receives a build request
(the `RealizeRequest` from [zbstorerpc.go](../zbstorerpc/zbstorerpc.go)): \
Let $O$ be the set of derivations requested. \
Let $`U = \bigcup_{o \in O} o \cup I^+(o)`$.

### Step 1: Load Graph

Load $U$ into memory.
If all derivations in $O$ exist in the store,
then $`\bigcup_{o \in O} I^+(o)`$ should exist in the store
because input derivations are store references.

### Step 2: Gather Existing Realizations

Walk $U$ in dependency order
(i.e. derivations with no input derivations first),
selecting realizations and [hashing derivations][].
Let $K$ be the set of derivations that have realizations for outputs used in $U$
based on the request's reuse policy.
New realizations may be downloaded from the fallback store
if an output does not have any trusted realizations
or an output does not have any local store paths.

Note that $`U \cap F \subseteq K`$ regardless of reuse policy:
fixed-output derivations' hashes are purely based on their output hashes.

[hashing derivations]: https://main--zb-docs.netlify.app/binary-cache/realizations#derivation-hashes

### Step 3: Obtain Build Roots

Let $D$ be the set of derivations whose realizations' outputs (based on $K$) are present locally
or can be downloaded successfully from the fallback store. Note that $`D \subseteq K`$.
This step is called "obtain build roots"
because checking whether a derivation is an element of $D$
has the potential side-effect of downloading an object into the local store from the fallback store.

```math
\begin{align*}
B \subseteq&\; U\\
B =&\; \bigcup_{o \in O} g(f(K), o) \\

\\

f :&\; \mathcal{P}(K) \to \mathcal{P}(K) \\
f(\mathcal{K}) =&\; \begin{cases}
  \mathcal{K} &\text{if } \mathcal{K} \cap \Big(\bigcup_{o \in O} g(\mathcal{K}, o) \setminus F\Big) = \emptyset \\
  f\bigg( \mathcal{K} \setminus \Big(\bigcup_{o \in O} g(\mathcal{K}, o) \setminus F\Big) \bigg) &\text{otherwise}
\end{cases} \\

\\

g :&\; \mathcal{P}(K) \times U \to \mathcal{P}(U) \\
g(\mathcal{K}, x) =&\; \begin{cases}
  \emptyset &\text{if } a(\mathcal{K}, x) \\
  \{x\} \cup \bigcup_{i \in I(x)} g(\mathcal{K}, i) &\text{otherwise}
\end{cases} \\

\\

a :&\; \mathcal{P}(K) \times U \to \mathbb{F}_2 \\
a(\mathcal{K}, x) =&\; x \in \mathcal{K} \land x \in D \land (x \in F \lor I(x) \subseteq \mathcal{K})

\end{align*}
```

- $B$ is the set of derivations to build &mdash;
  the output of the "obtain build roots" step.
- $f$ filters a set of derivations to the ones whose realizations can be reused.
- $g$ is a function that maps from a set of derivations whose realizations can be reused
  and a derivation $x$
  to the set of derivations that must be built to produce $x$.
- $a$ is a predicate for "output available".

### Step 4: Build What Remains

Walk $`B \cup O`$ in dependency order.
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
