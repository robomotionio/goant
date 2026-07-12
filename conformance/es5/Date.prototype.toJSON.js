function check(a,b){if(a!==b)throw a+" !== "+b;} check(new Date(0).toJSON(),'1970-01-01T00:00:00.000Z');console.log('PASS');
