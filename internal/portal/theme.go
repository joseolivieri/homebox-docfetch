package portal

// themeHead is the palette shared by the server-rendered pages (the activity
// log and the ntfy action confirmation). Those open in the browser rather than
// the installed PWA, but they are the same origin, so the inline script picks
// up the theme the user chose in the app; with no stored choice they follow
// the OS. Keep the token values in step with static/index.html.
const themeHead = `<style>
:root{color-scheme:light;
--bg:#f2f4f7;--surface:#ffffff;--border:#d6dae3;--text:#14161b;--muted:#58606e;
--link:#1d4ed8;--kind:#2e6f2e}
@media (prefers-color-scheme:dark){:root:not([data-theme="light"]){color-scheme:dark;
--bg:#111318;--surface:#1b1e26;--border:#2a2d35;--text:#e6e6e9;--muted:#9aa0ac;
--link:#7aa2f7;--kind:#c3e88d}}
:root[data-theme="dark"]{color-scheme:dark;
--bg:#111318;--surface:#1b1e26;--border:#2a2d35;--text:#e6e6e9;--muted:#9aa0ac;
--link:#7aa2f7;--kind:#c3e88d}
</style><script>
try{var t=localStorage.getItem('intake.theme');
if(t==='light'||t==='dark')document.documentElement.dataset.theme=t;}catch(e){}
</script>`
