function check(a,b){if(a!==b)throw a+" !== "+b;} var e=new Error('hello');check(e.message,'hello');check(new Error().message,'');console.log('PASS');
