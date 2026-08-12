# Chunk contextualisation

A chunk's embed input is prefixed with its source and section so two chunks sharing vocabulary but belonging to different documents do not collide in vector space. Stored content stays raw, so dedup and display are unaffected.
