\set ON_ERROR_STOP on

BEGIN ISOLATION LEVEL REPEATABLE READ READ ONLY;
SELECT 'BEGIN;';
SELECT 'DO $$ BEGIN IF EXISTS (SELECT 1 FROM eanation.accounts) THEN RAISE EXCEPTION ''target accounts table is not empty''; END IF; END $$;';

WITH accounts AS (
  SELECT a.*, row_number() OVER (PARTITION BY lower(email) ORDER BY id) AS email_number
  FROM community_gameaccount a
)
SELECT format(
  'INSERT INTO eanation.accounts(id,name,password_hash,mail,loc) OVERRIDING SYSTEM VALUE VALUES (%s,%L,%L,%L,%L);',
  id, name,
  'tsfp-pbkdf2-sha256$200000$' || replace(rtrim(encode(salt, 'base64'), '='), E'\n', '') || '$' ||
    replace(rtrim(encode(password_hash, 'base64'), '='), E'\n', ''),
  CASE WHEN email = '' OR email_number = 1 THEN email ELSE '' END,
  country
)
FROM accounts ORDER BY id;

SELECT format(
  'INSERT INTO eanation.personas(id,account_id,name) OVERRIDING SYSTEM VALUE VALUES (%s,%s,%L);',
  id, account_id, name
)
FROM community_persona ORDER BY id;

SELECT format(
  'INSERT INTO eanation.buddies(title,a,b,inviter,accepted) VALUES (''tsfp'',%L,%L,%L,%L);',
  least(low.name, high.name), greatest(low.name, high.name), requester.name, friendship.accepted
)
FROM community_friendship friendship
JOIN community_persona low ON low.id = friendship.low_id
JOIN community_persona high ON high.id = friendship.high_id
JOIN community_persona requester ON requester.id = friendship.requester_id
ORDER BY friendship.low_id, friendship.high_id;

SELECT format(
  'INSERT INTO eanation.mailbox(id,title,sender,recipient,kind,body,sent_at) OVERRIDING SYSTEM VALUE VALUES (%s,''tsfp'',%L,%L,%L,%L::bytea,to_timestamp(%s));',
  message.id, sender.name, recipient.name, 'tsfp-' || message.message_type,
  '\x' || encode(convert_to(message.subject, 'UTF8'), 'hex') || '00' ||
    encode(convert_to(message.message_body, 'UTF8'), 'hex'),
  message.created_at
)
FROM community_offlinemessage message
JOIN community_persona sender ON sender.id = message.sender_id
JOIN community_persona recipient ON recipient.id = message.recipient_id
ORDER BY message.id;

SELECT format(
  'INSERT INTO eanation.tsfp_maps(id,owner,name_key,digest,content,metadata,created_at,downloads,rating_sum,reviews) OVERRIDING SYSTEM VALUE VALUES (%s,%s,%L,%L::bytea,%L::bytea,%L::jsonb,%L::timestamptz,%s,%s,%s);',
  id, persona_id,
  lower(regexp_replace(btrim(metadata->>'name'), '\s+', ' ', 'g')) || '#legacy-' || id,
  '\x' || encode(sha256(content), 'hex'), '\x' || encode(content, 'hex'), metadata::text, created,
  download_count, round(rating * review_count)::bigint, review_count
)
FROM community_mapblob ORDER BY id;

SELECT format(
  'INSERT INTO eanation.tsfp_map_reviews(map_id,voter,rating) VALUES (%s,%s,%s);',
  blob_id, persona_id, rating
)
FROM community_maprating ORDER BY blob_id, persona_id;

SELECT format(
  'INSERT INTO eanation.tsfp_stats(persona,key,value,detail) VALUES (%s,%L,%L::numeric,%L);',
  owner_id, stat_key, value::text, text
)
FROM community_gamestatvalue
WHERE owner_type = 1 AND period_id = 0 AND period_past = 0
ORDER BY owner_id, stat_key;

SELECT format(
  'SELECT setval(pg_get_serial_sequence(%L,%L),coalesce((SELECT max(%I) FROM eanation.%I),1),EXISTS (SELECT 1 FROM eanation.%I));',
  'eanation.' || table_name, column_name, column_name, table_name, table_name
)
FROM (VALUES
  ('accounts','id'), ('personas','id'), ('mailbox','id'), ('tsfp_maps','id')
) AS sequences(table_name,column_name);

SELECT 'COMMIT;';
COMMIT;
