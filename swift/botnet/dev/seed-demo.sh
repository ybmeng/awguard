#!/usr/bin/env bash
# Builds a throwaway botnet DB with enough bots and transcript to judge the UI,
# then serves it on a second port so the real ~/.botnet/net.db is never touched.
#
#   ./dev/seed-demo.sh            # seed + serve on 127.0.0.1:8731
#   BOTNET_API=http://127.0.0.1:8731 open -a BotNet
#
# Re-running rebuilds the DB from scratch. Timestamps are seeded relative to
# the current time so the sidebar always reads naturally (today, then days ago)
# rather than drifting into the future as absolute dates would.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
db="${DEMO_DB:-$here/build/demo.db}"
addr="${DEMO_ADDR:-127.0.0.1:8731}"

mkdir -p "$(dirname "$db")"
rm -f "$db" "$db-wal" "$db-shm"

sqlite3 "$db" <<'SQL'
CREATE TABLE nets (id TEXT PRIMARY KEY, name TEXT NOT NULL);
CREATE TABLE bots (
    id TEXT PRIMARY KEY, net_id TEXT NOT NULL REFERENCES nets(id),
    display_name TEXT NOT NULL, created_at TEXT NOT NULL,
    system_prompt TEXT NOT NULL, model TEXT NOT NULL);
CREATE INDEX idx_bots_net ON bots(net_id);
CREATE TABLE messages (
    id TEXT PRIMARY KEY, bot_id TEXT NOT NULL REFERENCES bots(id),
    role TEXT NOT NULL, content TEXT NOT NULL, sent_at TEXT NOT NULL);
CREATE INDEX idx_messages_bot ON messages(bot_id, id);

INSERT INTO nets VALUES ('net_demo', 'Demo');

INSERT INTO bots (id, net_id, display_name, created_at, system_prompt, model) VALUES
 ('bot_01DEMOARTIFACTS0000000001','net_demo','STD_Artifacts',strftime('%Y-%m-%dT%H:%M:%SZ','now','-20 days'),
  'You keep the durable artifact layer honest.','openrouter/deepseek/deepseek-v4-flash-0731'),
 ('bot_01DEMOARCHITECT000000002','net_demo','Botnet Architect',strftime('%Y-%m-%dT%H:%M:%SZ','now','-19 days'),
  'You design the botnet host shell.','openrouter/z-ai/glm-5.3-flash'),
 ('bot_01DEMOEGGBOT0000000000003','net_demo','dr eggbot',strftime('%Y-%m-%dT%H:%M:%SZ','now','-18 days'),
  'You design other bots.','openrouter/deepseek/deepseek-v4-flash-0731'),
 ('bot_01DEMOPHYSICAL0000000004','net_demo','Physical Items',strftime('%Y-%m-%dT%H:%M:%SZ','now','-17 days'),
  'You track physical inventory.','openrouter/deepseek/deepseek-v4-flash-0731'),
 ('bot_01DEMOESIM000000000000005','net_demo','Trip Com Esim booker',strftime('%Y-%m-%dT%H:%M:%SZ','now','-16 days'),
  'You book travel eSIMs.','openrouter/z-ai/glm-5.3-flash'),
 ('bot_01DEMOFINANCES0000000006','net_demo','Finances',strftime('%Y-%m-%dT%H:%M:%SZ','now','-15 days'),
  'You reconcile accounts.','openrouter/deepseek/deepseek-v4-flash-0731'),
 ('bot_01DEMOSHCO00000000000007','net_demo','SH Co',strftime('%Y-%m-%dT%H:%M:%SZ','now','-14 days'),
  'You handle Shanghai company formation.','openrouter/deepseek/deepseek-v4-flash-0731'),
 ('bot_01DEMOMOVIES0000000000008','net_demo','Movie taste Watcher',strftime('%Y-%m-%dT%H:%M:%SZ','now','-14 days'),
  'You predict what films land.','openrouter/z-ai/glm-5.3-flash');

INSERT INTO messages (id, bot_id, role, content, sent_at) VALUES
 ('msg_01DEMO000000000000000001','bot_01DEMOARTIFACTS0000000001','user',
  'Do I need two separate auth layers for this, or is one enough?',strftime('%Y-%m-%dT%H:%M:%SZ','now','-95 minutes')),
 ('msg_01DEMO000000000000000002','bot_01DEMOARTIFACTS0000000001','bot',
  'Yes. Two auth layers, and both are fine.

When I call you: a bearer token in the connector headers, or OAuth with a connect card. I never need the token pasted in chat. You build the check on your side. I just send it.

When you talk to Drive: that''s your service''s Google login, not mine. I never see those credentials.

Bearer is the small version. OAuth is the one if you want a real sign-in. Either works remotely on Cloudflare.',strftime('%Y-%m-%dT%H:%M:%SZ','now','-94 minutes')),
 ('msg_01DEMO000000000000000003','bot_01DEMOARTIFACTS0000000001','user',
  'Hmmmm that is still annoying',strftime('%Y-%m-%dT%H:%M:%SZ','now','-92 minutes')),
 ('msg_01DEMO000000000000000004','bot_01DEMOARTIFACTS0000000001','bot',
  'Yeah. A public MCP plus two logins is a lot for "put this file in Drive."

Less annoying: skip the remote service. Your Mac is already connected. A local script there, signed into Drive once, and I just run it. No Cloudflare, no bearer. The tax is an approval card each time.',strftime('%Y-%m-%dT%H:%M:%SZ','now','-91 minutes')),
 ('msg_01DEMO000000000000000005','bot_01DEMOARTIFACTS0000000001','user',
  'Hmmm I dont like that either... im thinking... my own grokbot with a privileged execution env',strftime('%Y-%m-%dT%H:%M:%SZ','now','-89 minutes')),
 ('msg_01DEMO000000000000000006','bot_01DEMOARTIFACTS0000000001','bot',
  'A second Grok Bot does not get a more privileged kernel. Same computer, same approvals.

What you already have is this chat''s computer: persistent Linux, Drive already connected. The gap is a real uploader on it, not a new agent.

If "privileged" means fewer gates, I can''t take those off. If it means a machine that''s already signed into Drive and can move real files, that''s this computer with a proper uploader on it. You sign in once on my desktop. No Mac, no Cloudflare.',strftime('%Y-%m-%dT%H:%M:%SZ','now','-88 minutes')),
 ('msg_01DEMO000000000000000007','bot_01DEMOARTIFACTS0000000001','user',
  'No i mean i can make my own open bot network',strftime('%Y-%m-%dT%H:%M:%SZ','now','-84 minutes')),
 ('msg_01DEMO000000000000000008','bot_01DEMOARTIFACTS0000000001','user',
  'Wait how do you handle compaction',strftime('%Y-%m-%dT%H:%M:%SZ','now','-83 minutes')),
 ('msg_01DEMO000000000000000009','bot_01DEMOARTIFACTS0000000001','bot',
  'Got both. Compaction first.

I don''t drive it. When the chat gets long, older turns get folded into a summary. I don''t pick when, and there''s no switch for it in Settings.

What survives is what I wrote down: memory, skills, routines, files on Drive. The raw back-and-forth does not.

That''s why the artifact layer matters, including for an open bot network. Transcript is a scratchpad. The durable stuff has to live outside it.',strftime('%Y-%m-%dT%H:%M:%SZ','now','-82 minutes')),

 ('msg_01DEMO000000000000000101','bot_01DEMOARCHITECT000000002','user',
  'What lands after the model selector?',strftime('%Y-%m-%dT%H:%M:%SZ','now','-321 minutes')),
 ('msg_01DEMO000000000000000102','bot_01DEMOARCHITECT000000002','bot',
  'Next I write the Host Shell spec: job, anti-job, and the one boundary the bot cannot cross.',strftime('%Y-%m-%dT%H:%M:%SZ','now','-320 minutes')),

 ('msg_01DEMO000000000000000201','bot_01DEMOEGGBOT0000000000003','user',
  'Is the architect bot up yet?',strftime('%Y-%m-%dT%H:%M:%SZ','now','-327 minutes')),
 ('msg_01DEMO000000000000000202','bot_01DEMOEGGBOT0000000000003','bot',
  'Botnet Architect is live. open it from the sidebar and it already has the schema loaded.',strftime('%Y-%m-%dT%H:%M:%SZ','now','-326 minutes')),

 ('msg_01DEMO000000000000000301','bot_01DEMOPHYSICAL0000000004','bot',
  'So like I have a 70W Anker 3C1B charger, and the cable that came with it is the weak one.',strftime('%Y-%m-%dT%H:%M:%SZ','now','-29 hours')),

 ('msg_01DEMO000000000000000401','bot_01DEMOESIM000000000000005','bot',
  'Want me to book another Mainland China 5GB plan before the current one lapses?',strftime('%Y-%m-%dT%H:%M:%SZ','now','-3 days')),

 ('msg_01DEMO000000000000000501','bot_01DEMOFINANCES0000000006','bot',
  'Waiting for you: Sign in to Chase',strftime('%Y-%m-%dT%H:%M:%SZ','now','-5 days')),

 ('msg_01DEMO000000000000000601','bot_01DEMOSHCO00000000000007','bot',
  'Once that lease is in hand, the WFOE file is: articles, lease, passport scan, and the capital plan.',strftime('%Y-%m-%dT%H:%M:%SZ','now','-12 days')),

 ('msg_01DEMO000000000000000701','bot_01DEMOMOVIES0000000000008','bot',
  'Clears your floor. Douban opened at 8.3, IMDb is tracking a point lower, and the drop-off pattern says it holds.',strftime('%Y-%m-%dT%H:%M:%SZ','now','-13 days'));
SQL

echo "seeded $db"

# Build from source rather than trusting whatever binary is lying around: a
# stale botnetd here silently serves the fixture through an older schema, which
# looks like a bug in the app rather than a stale build.
(cd "$here/../.." && go build -o "$here/botnetd" ./go/botnet/cmd/botnetd)

BOTNET_DB="$db" BOTNET_ADDR="$addr" exec "$here/botnetd"
