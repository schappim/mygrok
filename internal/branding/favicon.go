// Package branding holds shared visual assets used by both mygrokd
// (admin / offline pages) and the mygrok CLI (`mygrok serve` fallback
// when the served directory lacks a favicon).
package branding

// FaviconSVG is the default favicon — FontAwesome circle-dollar in
// Tailwind rose-700 (#be123c). Renders cleanly on both light and dark
// browser chrome.
const FaviconSVG = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 640 640"><path fill="#be123c" d="M320 576C461.4 576 576 461.4 576 320C576 178.6 461.4 64 320 64C178.6 64 64 178.6 64 320C64 461.4 178.6 576 320 576zM298.5 208L368 208C381.3 208 392 218.7 392 232C392 245.3 381.3 256 368 256L298.5 256C288.3 256 280 264.3 280 274.5C280 283.8 286.9 291.7 296.2 292.8L349.8 299.5C383.1 303.7 408 331.9 408 365.5C408 402.2 378.2 432 341.5 432L264 432C250.7 432 240 421.3 240 408C240 394.7 250.7 384 264 384L341.5 384C351.7 384 360 375.7 360 365.5C360 356.2 353.1 348.3 343.8 347.2L290.2 340.5C256.9 336.3 232 308.1 232 274.5C232 237.8 261.8 208 298.5 208z"/></svg>`
