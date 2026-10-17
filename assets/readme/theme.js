// The diagrams take their theme from the URL (?theme=dark|light); the README shows both in a
// <picture> so GitHub picks the one matching the reader's colour scheme.
document.documentElement.dataset.theme = new URLSearchParams(location.search).get("theme") || "dark";
