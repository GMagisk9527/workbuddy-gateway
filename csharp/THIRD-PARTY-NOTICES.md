# Third-party assets

`http/web/qrcodegen.js` is Project Nayuki's QR Code generator library, licensed under the MIT License. Its full copyright and permission notice is retained at the top of the vendored file.

Source: https://www.nayuki.io/res/qr-code-generator-library/qrcodegen.js

Vendored SHA-256: `2511bc17f40a3c41d4a0578995db956b38997334d3d20113a5d4dc5c49c69480`.

The library runs locally in the admin browser to render the upstream authorization URL. It does not contact an external QR generation service.

`zxing-cpp` is used only by the isolated QR decoding verification environment; it is not a runtime dependency or included in the server release.
