try {
  const saved = localStorage.getItem("color-scheme");
  if (saved === "light" || saved === "dark") {
    const meta = document.querySelector('meta[name="color-scheme"]');
    if (meta) meta.content = saved;
    document.documentElement.dataset.colorScheme = saved;
  }
} catch {}
