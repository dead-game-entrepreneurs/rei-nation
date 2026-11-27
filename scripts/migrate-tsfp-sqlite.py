#!/usr/bin/env python3
"""Emit a transactional PostgreSQL import for the legacy TSFP SQLite state."""

import argparse
import base64
import hashlib
import json
import sqlite3
import sys


def sql_text(value):
    return "'" + str(value).replace("'", "''") + "'"


def sql_bytes(value):
    return "'\\x" + bytes(value).hex() + "'::bytea"


def legacy_hash(salt, digest):
    encoded_salt = base64.b64encode(bytes(salt)).decode().rstrip("=")
    encoded_digest = base64.b64encode(bytes(digest)).decode().rstrip("=")
    return f"tsfp-pbkdf2-sha256$200000${encoded_salt}${encoded_digest}"


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("sqlite", help="consistent accounts.sqlite3 backup")
    args = parser.parse_args()
    db = sqlite3.connect(f"file:{args.sqlite}?mode=ro", uri=True)
    db.row_factory = sqlite3.Row
    out = sys.stdout

    print("BEGIN;", file=out)
    print("DO $$ BEGIN IF EXISTS (SELECT 1 FROM eanation.accounts) THEN RAISE EXCEPTION 'target accounts table is not empty'; END IF; END $$;", file=out)

    seen_mail = set()
    for row in db.execute("SELECT * FROM accounts ORDER BY id"):
        password = legacy_hash(row["salt"], row["password_hash"])
        mail = row["email"]
        mail_key = mail.casefold()
        if mail and mail_key in seen_mail:
            mail = ""
        elif mail:
            seen_mail.add(mail_key)
        print(
            "INSERT INTO eanation.accounts(id,name,password_hash,mail,loc) OVERRIDING SYSTEM VALUE VALUES "
            f"({row['id']},{sql_text(row['name'])},{sql_text(password)},{sql_text(mail)},{sql_text(row['country'])});",
            file=out,
        )

    personas = {row["id"]: row for row in db.execute("SELECT * FROM personas ORDER BY id")}
    for row in personas.values():
        print(
            "INSERT INTO eanation.personas(id,account_id,name) OVERRIDING SYSTEM VALUE VALUES "
            f"({row['id']},{row['account_id']},{sql_text(row['name'])});",
            file=out,
        )

    for row in db.execute("SELECT * FROM friendships ORDER BY low_id,high_id"):
        left = personas[row["low_id"]]["name"]
        right = personas[row["high_id"]]["name"]
        a, b = sorted((left, right))
        inviter = personas[row["requester_id"]]["name"]
        accepted = "true" if row["accepted"] else "false"
        print(
            "INSERT INTO eanation.buddies(title,a,b,inviter,accepted) VALUES "
            f"('tsfp',{sql_text(a)},{sql_text(b)},{sql_text(inviter)},{accepted});",
            file=out,
        )

    for row in db.execute("SELECT * FROM offline_messages ORDER BY id"):
        sender = personas[row["sender_id"]]["name"]
        recipient = personas[row["recipient_id"]]["name"]
        body = (row["subject"] + "\0" + row["message_body"]).encode()
        print(
            "INSERT INTO eanation.mailbox(id,title,sender,recipient,kind,body,sent_at) OVERRIDING SYSTEM VALUE VALUES "
            f"({row['id']},'tsfp',{sql_text(sender)},{sql_text(recipient)},"
            f"{sql_text('tsfp-' + row['message_type'])},{sql_bytes(body)},to_timestamp({row['created_at']}));",
            file=out,
        )

    seen_map_names = set()
    for row in db.execute("SELECT * FROM map_blobs ORDER BY id"):
        content = bytes(row["content"])
        metadata = json.loads(row["metadata"])
        name_key = " ".join(str(metadata["name"]).split()).casefold()
        if name_key in seen_map_names:
            name_key = f"{name_key}#legacy-{row['id']}"
        seen_map_names.add(name_key)
        rating_sum = round(float(row["rating"]) * int(row["review_count"]))
        print(
            "INSERT INTO eanation.tsfp_maps(id,owner,name_key,digest,content,metadata,created_at,downloads,rating_sum,reviews) "
            "OVERRIDING SYSTEM VALUE VALUES "
            f"({row['id']},{row['persona_id']},{sql_text(name_key)},{sql_bytes(hashlib.sha256(content).digest())},"
            f"{sql_bytes(content)},{sql_text(json.dumps(metadata, separators=(',', ':')))}::jsonb,"
            f"{sql_text(row['created'])}::timestamptz,{row['download_count']},{rating_sum},{row['review_count']});",
            file=out,
        )

    for row in db.execute("SELECT * FROM map_ratings ORDER BY blob_id,persona_id"):
        print(
            "INSERT INTO eanation.tsfp_map_reviews(map_id,voter,rating) VALUES "
            f"({row['blob_id']},{row['persona_id']},{row['rating']});",
            file=out,
        )

    for table, column in (("accounts", "id"), ("personas", "id"), ("mailbox", "id"), ("tsfp_maps", "id")):
        print(
            f"SELECT setval(pg_get_serial_sequence('eanation.{table}','{column}'),"
            f"coalesce((SELECT max({column}) FROM eanation.{table}),1),"
            f"EXISTS (SELECT 1 FROM eanation.{table}));",
            file=out,
        )
    print("COMMIT;", file=out)


if __name__ == "__main__":
    main()
