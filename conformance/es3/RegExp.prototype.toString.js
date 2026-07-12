function check(a,b){if(a!==b)throw a+" !== "+b;} check(/abc/gi.toString(),'/abc/gi');check(new RegExp('x').toString(),'/x/');console.log('PASS');
