# Embed queue leasing

The embed queue was at-most-once: DequeueEmbedBatch deleted its queue rows and committed, then embedded and stored. A restart in that window left chunks with no queue row, no DLQ row and no embedding. Leasing makes the claim visible so a restart re-delivers the batch.
