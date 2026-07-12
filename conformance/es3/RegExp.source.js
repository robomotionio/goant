function check(a,b){if(a!==b)throw a+" !== "+b;} check(/abc/.source,'abc');check(new RegExp('x+').source,'x+');console.log('PASS');
