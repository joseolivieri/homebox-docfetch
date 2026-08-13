// Shared shell behaviour for every portal page. Loaded from <head> AFTER
// app.css, so it can read the resolved palette and so the stored theme is
// applied before the first paint.

// Theme: follow the OS, or override it per device. The choice lives in
// localStorage — an installed PWA keeps its own storage per origin, so this is
// a device-local preference with no account and no server round trip.
// sessionStorage would not survive relaunching the app. "auto" is stored
// explicitly rather than as an absent key, so changing the default later
// cannot silently flip someone who chose to follow their OS.
const THEMES = ['auto', 'light', 'dark'];
const prefersDark = matchMedia('(prefers-color-scheme: dark)');

// Monochrome silhouettes, sized by CSS and filled with currentColor.
const THEME_ICON = {
  auto: '<svg viewBox="0 0 24 24" aria-hidden="true"><path fill-rule="evenodd" d="M12 2a10 10 0 100 20 10 10 0 000-20zm0 2a8 8 0 110 16 8 8 0 010-16z"/><path d="M12 5.5a6.5 6.5 0 000 13z"/></svg>',
  light: '<svg viewBox="0 0 24 24" aria-hidden="true"><circle cx="12" cy="12" r="5"/><path d="M11 1h2v3.5h-2zM11 19.5h2V23h-2zM1 11h3.5v2H1zM19.5 11H23v2h-3.5zM3.5 4.9l1.4-1.4 2.5 2.5-1.4 1.4zM16.6 18l1.4-1.4 2.5 2.5-1.4 1.4zM18 7.4L16.6 6l2.5-2.5 1.4 1.4zM5.9 20.5l-1.4-1.4L7 16.6l1.4 1.4z"/></svg>',
  dark: '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M12.4 3a9 9 0 108.6 11.4A7.5 7.5 0 0112.4 3z"/></svg>',
};

function currentTheme() { return document.documentElement.dataset.theme || 'auto'; }

function applyTheme(t) {
  if (t === 'auto') delete document.documentElement.dataset.theme;
  else document.documentElement.dataset.theme = t;
  try { localStorage.setItem('intake.theme', t); } catch (e) {} // private mode
  // Keep the iOS status bar and PWA chrome in step with the page. Read back
  // the resolved token so it can never drift from the stylesheet.
  const meta = document.querySelector('meta[name=theme-color]');
  if (meta) meta.content = getComputedStyle(document.documentElement)
    .getPropertyValue('--bg').trim();
  for (const b of document.querySelectorAll('.themebtn')) {
    b.innerHTML = THEME_ICON[t];
    b.title = 'Theme: ' + t;
    b.setAttribute('aria-label', 'Theme: ' + t + '. Tap to change.');
  }
}

function cycleTheme() {
  applyTheme(THEMES[(THEMES.indexOf(currentTheme()) + 1) % THEMES.length]);
}

// Apply the stored choice now, before <body> renders, so a light choice never
// flashes dark on launch. The buttons are painted once the DOM exists.
(function () {
  let t = null;
  try { t = localStorage.getItem('intake.theme'); } catch (e) {}
  if (t === 'light' || t === 'dark') document.documentElement.dataset.theme = t;
})();
document.addEventListener('DOMContentLoaded', () => applyTheme(currentTheme()));

// While on auto, an OS switch has to repaint the status bar too.
prefersDark.addEventListener('change', () => { if (currentTheme() === 'auto') applyTheme('auto'); });

// iOS Safari ignores user-scalable=no, so pinch-zoom has to be refused here
// for the page to feel like an app rather than a document.
document.addEventListener('gesturestart', e => e.preventDefault());
