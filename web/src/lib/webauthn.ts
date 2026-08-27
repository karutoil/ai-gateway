// WebAuthn helpers for passkey registration/login
export function b64urlToBuf(b64url: string): ArrayBuffer {
  const pad = '='.repeat((4 - (b64url.length % 4)) % 4);
  const b64 = (b64url + pad).replace(/-/g, '+').replace(/_/g, '/');
  const str = atob(b64);
  const buf = new Uint8Array(str.length);
  for (let i=0;i<str.length;i++) buf[i]=str.charCodeAt(i);
  return buf.buffer as ArrayBuffer;
}
export function bufToB64url(buf: ArrayBuffer): string {
  const bytes = new Uint8Array(buf);
  let str = '';
  for (const b of bytes) str += String.fromCharCode(b);
  return btoa(str).replace(/\+/g,'-').replace(/\//g,'_').replace(/=/g,'');
}
export function toArrayBuffer(v: any): ArrayBuffer {
  if (v instanceof ArrayBuffer) return v;
  if (ArrayBuffer.isView(v)) return (v as Uint8Array).buffer.slice((v as Uint8Array).byteOffset, (v as Uint8Array).byteOffset + (v as Uint8Array).byteLength) as ArrayBuffer;
  if (typeof v === 'string') return b64urlToBuf(v);
  return v as ArrayBuffer;
}

// Convert server options (with base64url) to browser PublicKeyCredentialCreationOptions
export function decodeCreateOptions(opts: any): PublicKeyCredentialCreationOptions {
  const o = opts.options || opts;
  // Handle nested publicKey
  const pk = o.publicKey || o;
  return {
    challenge: toArrayBuffer(pk.challenge),
    rp: pk.rp,
    user: {
      id: toArrayBuffer(pk.user.id),
      name: pk.user.name,
      displayName: pk.user.displayName,
    },
    pubKeyCredParams: pk.pubKeyCredParams,
    timeout: pk.timeout,
    attestation: pk.attestation,
    authenticatorSelection: pk.authenticatorSelection,
    excludeCredentials: (pk.excludeCredentials||[]).map((c:any)=>({...c, id: toArrayBuffer(c.id)})),
    extensions: pk.extensions,
  } as PublicKeyCredentialCreationOptions;
}

export function decodeRequestOptions(opts: any): PublicKeyCredentialRequestOptions {
  const o = opts.options || opts;
  const pk = o.publicKey || o;
  return {
    challenge: toArrayBuffer(pk.challenge),
    timeout: pk.timeout,
    rpId: pk.rpId,
    allowCredentials: (pk.allowCredentials||[]).map((c:any)=>({...c, id: toArrayBuffer(c.id)})),
    userVerification: pk.userVerification,
    extensions: pk.extensions,
  } as PublicKeyCredentialRequestOptions;
}

export function encodeCredential(cred: PublicKeyCredential): any {
  const c = cred as any;
  const resp = c.response;
  // Helper to get ArrayBuffer
  const getBuf = (v:any):ArrayBuffer | null => {
    if (!v) return null;
    if (v instanceof ArrayBuffer) return v;
    if (ArrayBuffer.isView(v)) return (v as Uint8Array).buffer.slice((v as Uint8Array).byteOffset, (v as Uint8Array).byteOffset + (v as Uint8Array).byteLength) as ArrayBuffer;
    return null;
  };
  return {
    id: c.id,
    rawId: bufToB64url(c.rawId),
    type: c.type,
    response: {
      attestationObject: resp.attestationObject ? bufToB64url(resp.attestationObject) : undefined,
      clientDataJSON: resp.clientDataJSON ? bufToB64url(resp.clientDataJSON) : undefined,
      authenticatorData: resp.authenticatorData ? bufToB64url(resp.authenticatorData) : undefined,
      signature: resp.signature ? bufToB64url(resp.signature) : undefined,
      userHandle: resp.userHandle ? bufToB64url(resp.userHandle) : undefined,
    },
    clientExtensionResults: c.getClientExtensionResults ? c.getClientExtensionResults() : {},
  };
}

export async function registerPasskey(beginRes: any): Promise<{session:string, credential:any}> {
  const session = beginRes.session;
  const options = decodeCreateOptions(beginRes);
  const cred = await navigator.credentials.create({ publicKey: options }) as PublicKeyCredential;
  if (!cred) throw new Error('no credential');
  const encoded = encodeCredential(cred);
  return { session, credential: encoded };
}

export async function authenticatePasskey(beginRes: any): Promise<{session:string, credential:any}> {
  const session = beginRes.session;
  const options = decodeRequestOptions(beginRes);
  const cred = await navigator.credentials.get({ publicKey: options }) as PublicKeyCredential;
  if (!cred) throw new Error('no credential');
  const c = cred as any;
  const resp = c.response;
  const encoded = {
    id: c.id,
    rawId: bufToB64url(c.rawId),
    type: c.type,
    response: {
      authenticatorData: resp.authenticatorData ? bufToB64url(resp.authenticatorData) : undefined,
      clientDataJSON: resp.clientDataJSON ? bufToB64url(resp.clientDataJSON) : undefined,
      signature: resp.signature ? bufToB64url(resp.signature) : undefined,
      userHandle: resp.userHandle ? bufToB64url(resp.userHandle) : undefined,
    },
    clientExtensionResults: c.getClientExtensionResults ? c.getClientExtensionResults() : {},
  };
  return { session, credential: encoded };
}
