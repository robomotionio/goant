function check(a,b){if(a!==b)throw a+" !== "+b;} check(typeof URIError,'function');check(new URIError() instanceof Error,true);console.log('PASS');
