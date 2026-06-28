/* C2 long-term device identity (ECDSA P-256, Doc 15).
 *
 * The device holds an ECDSA P-256 keypair; the backend stores only the public key, so a backend
 * breach cannot forge a device. The private scalar never leaves the chip. Used to sign the re-key
 * handshake that establishes a per-session HOTP secret (see cmd.c). Key material in NVS namespace
 * "picpak": c2_sk (32 B private scalar) + c2_pk (65 B uncompressed public point). */
#pragma once
#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>

/* True if a keypair has been bonded (a private key exists in NVS). */
bool c2_key_present(void);

/* Ensure a keypair exists (generate + persist on first call = bonding), then write the 65-byte
 * uncompressed public point (0x04||X||Y) into out (cap >= 65) and set *outlen. False on error. */
bool c2_key_pubkey(uint8_t *out, size_t cap, size_t *outlen);

/* ECDSA P-256 sign over sha256(msg[0..msglen)). Writes an ASN.1 DER signature into sig (cap >= 80,
 * P-256 DER is <= 72 B) and sets *siglen. False if no keypair is provisioned or on error. */
bool c2_key_sign(const uint8_t *msg, size_t msglen, uint8_t *sig, size_t cap, size_t *siglen);
