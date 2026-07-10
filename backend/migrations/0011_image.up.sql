-- 0011_image.up.sql — content-addressed source-image store (A27 D1/D2 foundation, W1).
-- Policy=Data. Idempotent (IF NOT EXISTS) + independent + re-runnable against the final schema.
-- // TODO(multi-tenant): owner/tenant column + composite keys (foundation M2 seam, parity 0008/0009).
CREATE TABLE IF NOT EXISTS image (
    id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    operator_key_id BIGINT REFERENCES operator_keys(id),   -- attribution, FK RESTRICT (parity 0007:21-22)
    sha256          TEXT   NOT NULL UNIQUE,                 -- content address, dedup key; 64 lc hex
                                                            -- // TODO(multi-tenant): -> UNIQUE(operator_key_id, sha256) per-operator namespace (§5, K-MT)
    blob_path       TEXT   NOT NULL,                        -- sha256||'.bin' in the imgblobs volume; Serve/Delete derive the FS
                                                            -- path from the CHECKed sha256, NOT from this free column (§4.1 traversal defense)
    mime            TEXT   NOT NULL,                        -- image/png|jpeg (server-sniffed via image.DecodeConfig, not client)
    width           INT    NOT NULL,                        -- server-sniffed; pixel-dim cap enforced in PutImage (decompression bomb §5)
    height          INT    NOT NULL,
    byte_size       BIGINT NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT image_sha_hex  CHECK (sha256 ~ '^[0-9a-f]{64}$'),
    CONSTRAINT image_size_pos CHECK (byte_size > 0)
);
