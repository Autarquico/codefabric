# Specs

One spec per milestone, managed with `/pm` (frontmatter: `codename`, `title`, `type`, `status`, `priority`, `size`, `depends_on`, …).

Each spec contains:

1. **Goal** — what the milestone delivers and the sections of `docs/codefabric-spec.md` it implements.
2. **Scope** — packages, endpoints, commands.
3. **Scenarios** — Given/When/Then. Each one becomes a `TestScenario_*` test.
4. **Out of scope** — what is explicitly deferred.
