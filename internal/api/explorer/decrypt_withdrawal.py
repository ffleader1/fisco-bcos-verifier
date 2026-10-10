#!/usr/bin/env python3
"""Decrypt a BuoyUp withdrawal with your own key, without trusting the BuoyUp server.

    pip install cryptography
    python3 decrypt_withdrawal.py <server> <withdrawal id or tx hash> <your private key hex>
    python3 decrypt_withdrawal.py http://localhost:12080 WD-ED46743BC5E11650 3f1c...e9

It downloads the PUBLIC on-chain record (envelope, commitments, root) from the explorer, decrypts on your
machine, and checks every piece you can read against its on-chain commitment and the Merkle root.
Format: see internal/sealing/sealing.go.
"""
import base64, hashlib, json, sys, urllib.request

from cryptography.hazmat.primitives import hashes
from cryptography.hazmat.primitives.asymmetric.x25519 import X25519PrivateKey, X25519PublicKey
from cryptography.hazmat.primitives.ciphers.aead import AESGCM
from cryptography.hazmat.primitives.kdf.hkdf import HKDF
from cryptography.hazmat.primitives.serialization import Encoding, PublicFormat


def merkle_root(leaves):
    level = list(leaves)
    while len(level) > 1:
        nxt = [hashlib.sha256(level[i] + level[i + 1]).digest() for i in range(0, len(level) - 1, 2)]
        if len(level) % 2:
            nxt.append(level[-1])  # odd node moves up unchanged
        level = nxt
    return level[0]


def main(server, wid, priv_hex):
    url = server.rstrip("/") + "/explorer/api/withdrawals/" + wid
    record = json.load(urllib.request.urlopen(url))["withdrawal"]
    wid = record["withdrawalId"]
    env = record["envelope"]
    commitments = [bytes.fromhex(c[2:]) for c in record["commitments"]]
    print(f"withdrawal {wid}: {len(env['pieces'])} encrypted pieces, {len(env['slots'])} key slots")

    root_ok = merkle_root(commitments) == bytes.fromhex(record["root"][2:])
    print("Merkle root of the commitments matches the on-chain root:", root_ok)

    priv = X25519PrivateKey.from_private_bytes(bytes.fromhex(priv_hex.removeprefix("0x")))
    my_pub = priv.public_key().public_bytes(Encoding.Raw, PublicFormat.Raw)
    eph = bytes.fromhex(env["eph"])
    shared = priv.exchange(X25519PublicKey.from_public_bytes(eph))
    slot_key = HKDF(algorithm=hashes.SHA256(), length=32, salt=eph + my_pub,
                    # frozen label: the old "BouyUp" spelling is part of the encryption format
                    info=("BouyUp withdrawal v1 " + wid).encode()).derive(shared)

    for slot in env["slots"]:  # slots carry no names: try each, AES-GCM rejects the wrong ones
        try:
            plain = AESGCM(slot_key).decrypt(bytes.fromhex(slot["nonce"]), base64.b64decode(slot["ct"]), (wid + "|slot").encode())
        except Exception:
            continue
        for ref in json.loads(plain)["keys"]:
            i = ref["piece"]
            box = env["pieces"][i]
            piece = AESGCM(bytes.fromhex(ref["key"])).decrypt(bytes.fromhex(box["nonce"]), base64.b64decode(box["ct"]), f"{wid}|{i}".encode())
            ok = hashlib.sha256(piece).digest() == commitments[i]
            print(f"\npiece {i} (matches on-chain commitment: {ok})")
            print(json.dumps(json.loads(piece), indent=2, ensure_ascii=False))
        return 0
    print("\nThis key opens none of the slots: it may not read this withdrawal.")
    return 1


if __name__ == "__main__":
    if len(sys.argv) != 4:
        print(__doc__)
        sys.exit(2)
    sys.exit(main(*sys.argv[1:]))
