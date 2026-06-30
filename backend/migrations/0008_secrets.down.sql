-- 0008 down: drop the sealed secrets KV. No existing ciphertext to preserve (fresh deployment).
DROP TABLE IF EXISTS secrets;
