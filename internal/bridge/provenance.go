package bridge

// Public-post provenance contract (V2-H26).
//
// Scheduled composers (diary, 占卜, 月光小思考, briefings) write into the
// family group — a PUBLIC surface. On 7/5 a fact told to one agent in a
// private DM surfaced in the peer's public post;
// on 7/10 a zodiac sign was improvised wrong; on 7/6 yesterday's dove-nest
// state was published as today's. The contract below is injected into every
// system-initiated turn that targets a group chat. Memory writes now carry
// chat:<id> provenance tags (memory.go) so the origin of a fact is checkable.
const publicPostContract = `[PUBLIC-POST CONTRACT — this scheduled turn publishes to the FAMILY GROUP (a public surface):
- Use only facts learned IN THIS GROUP, or clearly non-private knowledge. Anything a family member told you in a private DM must NOT appear here — that is a privacy breach, and it has happened before. When unsure where you learned something, leave it out.
- Person-facts (birthdays, zodiac signs, preferences) must match your pinned memories exactly. Verify before writing — never improvise a person-fact.
- Date-sensitive states (nests, plants, health, plans): assert today's state only if you learned it today; otherwise phrase it as last-known (「昨天看到…」).]`
