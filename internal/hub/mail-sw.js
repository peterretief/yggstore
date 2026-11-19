// New mail notices for "Your mail" (pushmail.go): the box sends a notice,
// and this shows it, and opens the mail page when it's pressed.
self.addEventListener("push", e => {
  let d = {};
  try { d = e.data ? e.data.json() : {}; } catch (_) {}
  e.waitUntil(self.registration.showNotification(d.title || "New mail", {
    body: d.body || "", tag: "mail", renotify: true, data: {url: d.url || "/files/mail"}}));
});
self.addEventListener("notificationclick", e => {
  e.notification.close();
  const url = (e.notification.data && e.notification.data.url) || "/files/mail";
  e.waitUntil(clients.matchAll({type: "window", includeUncontrolled: true}).then(ws => {
    for (const w of ws) if (new URL(w.url).pathname === url && "focus" in w) return w.focus();
    return clients.openWindow(url);
  }));
});
