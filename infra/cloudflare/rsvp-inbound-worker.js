// Cloudflare Email Worker: relays RSVP mail for invites@h2oflows.app to the
// api's inbound webhook. Deployed by hand: Workers & Pages -> create worker,
// paste this file; Email Routing -> rule invites@h2oflows.app -> this worker.
// Vars (Settings -> Variables): RSVP_HOOK_URL = https://api.h2oflows.app/api/v1/hooks/rsvp-inbound
// RSVP_HOOK_SECRET = same value as the api's RSVP_INBOUND_SECRET (secret var).
export default {
  async email(message, env, ctx) {
    try {
      const resp = await fetch(env.RSVP_HOOK_URL, {
        method: 'POST',
        headers: {
          'X-RSVP-Secret': env.RSVP_HOOK_SECRET,
          'Content-Type': 'message/rfc822',
          'X-Envelope-From': message.from,
          'X-Envelope-To': message.to,
        },
        body: message.raw,
      })
      if (!resp.ok) console.log('rsvp relay non-ok', resp.status)
    } catch (err) {
      // Swallow: a relay failure must not bounce the sender's mail client.
      console.log('rsvp relay failed', String(err))
    }
  },
}
